#!/usr/bin/env bash
# sync-upstream.sh — 检查 + 同步 + 合并 + 验证
#
# 流程:
#   1. 校验工作区干净（不干净直接退出，避免覆盖未提交改动）
#   2. gh repo sync <fork>                       # 远程 fork 默认分支 ← parent（走 API）
#   3. git checkout <base> && git pull --ff-only # 本地基线分支 ← origin（SSH）
#   4. 切回原分支，git merge <base>              # 冲突则停下，不自动解决
#   5. go build / go vet / go test ./...
#   6. 合并后人工核对项（前端路由 / 元素 id / JS 重复声明）
#
# 用法:
#   ./scripts/sync-upstream.sh          # 到第 6 步为止（不推送）
#   ./scripts/sync-upstream.sh --push   # 验证通过后把当前分支推送到 origin
#
# 环境自愈（无需手工处理，脚本自行检测）:
#   · 残缺的 GIT_CONFIG_*（缺 GIT_CONFIG_KEY_n）会让所有 git 命令 fatal → 自动清除
#   · GOMODCACHE / GOCACHE 落在不可写路径（如沙箱外的 $HOME）→ 回退到 GO_CACHE_ROOT
#   · GOPROXY 不可达（proxy.golang.org 超时）→ 自动切换到可达镜像
#
# 可覆盖的环境变量:
#   FORK_REPO / PARENT_REPO / BASE_BRANCH / GO_BIN
#   GO_CACHE_ROOT  缓存回退根目录，默认 ${TMPDIR:-/tmp}/dsh-go
#
# 退出码: 0 = 成功；非 0 = 任一步骤失败（含合并冲突）。
set -euo pipefail

export PATH="$HOME/.local/bin:$PATH"

# ---------- 环境防御：残缺的 GIT_CONFIG_* ----------
# 坑: harness 可能只注入 GIT_CONFIG_COUNT 与 GIT_CONFIG_VALUE_0，却漏掉配对的
#     GIT_CONFIG_KEY_0。git 在**配置解析阶段**就 fatal，于是任何 git 子命令
#     （哪怕 git status）全部失败，报 "missing config key GIT_CONFIG_KEY_0"。
#     检测到不完整就整组清除。（若确实需要 codeg 的凭据助手，应补上
#     GIT_CONFIG_KEY_0=credential.helper，而不是清除。）
git_env_broken=0
if [ "${GIT_CONFIG_COUNT:-0}" -gt 0 ] 2>/dev/null; then
  i=0
  while [ "$i" -lt "$GIT_CONFIG_COUNT" ]; do
    key_var="GIT_CONFIG_KEY_$i"; val_var="GIT_CONFIG_VALUE_$i"
    if [ -z "${!key_var:-}" ] || [ -z "${!val_var:-}" ]; then git_env_broken=1; break; fi
    i=$((i + 1))
  done
fi
if [ "$git_env_broken" = 1 ]; then
  echo "提示: GIT_CONFIG_* 注入不完整（缺 GIT_CONFIG_KEY_<n>），已临时清除以免 git 报错。" >&2
  i=0
  while [ "$i" -lt "$GIT_CONFIG_COUNT" ]; do
    unset "GIT_CONFIG_KEY_$i" "GIT_CONFIG_VALUE_$i"
    i=$((i + 1))
  done
  unset GIT_CONFIG_COUNT
fi

sep() { printf '\n=== %s ===\n' "$*"; }
die() { echo "错误: $*" >&2; exit 1; }

PUSH=0
for arg in "$@"; do
  case "$arg" in
    --push) PUSH=1 ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "未知参数: $arg" ;;
  esac
done

# ---------- Go 工具链探测（本环境 go 不在默认 PATH） ----------
GO_BIN="${GO_BIN:-}"
if [ -z "$GO_BIN" ]; then
  for c in "$HOME/.local/toolchain/go/bin/go" "$(command -v go 2>/dev/null || true)"; do
    if [ -n "$c" ] && [ -x "$c" ]; then GO_BIN="$c"; break; fi
  done
fi
[ -n "$GO_BIN" ] || die "未找到 go（可设 GO_BIN=/path/to/go）"

# ---------- Go 构建环境自愈 ----------
# 坑 1: 沙箱下 GOMODCACHE / GOCACHE 默认落在 $HOME（工作区之外，不可写）。
#       go 读不到缓存会静默改为联网下载，表现成一堆 permission denied + 超时。
# 坑 2: proxy.golang.org 在部分网络不可达，不换镜像则编译直接失败。
go_env_changed=0

probe_writable() {
  # 整组重定向 stderr：否则 `> file` 失败时 shell 会先报 "Permission denied"，
  # 此时函数内的 2>/dev/null 还没生效，会在输出里留下噪音。
  {
    mkdir -p "$1" || return 1
    : > "$1/.dsh-write-probe" || return 1
    rm -f "$1/.dsh-write-probe"
  } 2>/dev/null
}

go_cache_root="${GO_CACHE_ROOT:-${TMPDIR:-/tmp}/dsh-go}"
for pair in "GOMODCACHE:modcache" "GOCACHE:buildcache"; do
  gv="${pair%%:*}"; gsub="${pair##*:}"
  gcur="$("$GO_BIN" env "$gv" 2>/dev/null || true)"
  if [ -z "$gcur" ] || ! probe_writable "$gcur"; then
    gfallback="$go_cache_root/$gsub"
    probe_writable "$gfallback" \
      || die "$gv 与回退路径均不可写: '$gcur' / '$gfallback'（可用 GO_CACHE_ROOT 指定）"
    export "$gv=$gfallback"
    go_env_changed=1
    echo "· $gv 不可写（$gcur）→ 回退到 $gfallback"
  fi
done

# 取 GOPROXY 列表里第一个 http(s) 条目（列表以 , 或 | 分隔）
first_proxy() {
  local rest="${1//|/,}" item
  while [ -n "$rest" ]; do
    item="${rest%%,*}"
    case "$item" in http://*|https://*) printf '%s' "$item"; return 0 ;; esac
    [ "$item" = "$rest" ] && return 1
    rest="${rest#*,}"
  done
  return 1
}

probe_url() { curl -sS -o /dev/null -m 6 "$1" >/dev/null 2>&1; }

if ! command -v curl >/dev/null 2>&1; then
  echo "· 未找到 curl，跳过 GOPROXY 可达性探测"
elif goproxy_first="$(first_proxy "$("$GO_BIN" env GOPROXY 2>/dev/null || true)")" \
     && ! probe_url "$goproxy_first"; then
  for mirror in https://goproxy.cn https://goproxy.io https://mirrors.aliyun.com/goproxy; do
    if probe_url "$mirror"; then
      export GOPROXY="$mirror,direct"
      # 校验库 sum.golang.org 通常同样不可达；go.sum 仍会校验已记录模块的哈希
      export GOSUMDB=off
      go_env_changed=1
      echo "· GOPROXY $goproxy_first 不可达 → 改用 $mirror"
      break
    fi
  done
fi

if [ "$go_env_changed" = 1 ]; then
  echo "· 生效的 Go 环境: GOMODCACHE=$("$GO_BIN" env GOMODCACHE) GOCACHE=$("$GO_BIN" env GOCACHE) GOPROXY=$("$GO_BIN" env GOPROXY)"
fi

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || true)"
[ -n "$repo_root" ] || die "当前目录不是 git 仓库"
cd "$repo_root"

# ---------- fork / parent 探测 ----------
# 坑: 同时有 origin(fork) 与 upstream(parent) 时 `gh repo view` 会解析到 upstream，
#     所以一律从 origin 的 URL 解析 fork。
parse_owner_repo() {
  local url="${1%.git}"
  case "$url" in
    git@github.com:*)       printf '%s' "${url#git@github.com:}" ;;
    ssh://git@github.com/*) printf '%s' "${url#ssh://git@github.com/}" ;;
    https://github.com/*)   printf '%s' "${url#https://github.com/}" ;;
    http://github.com/*)    printf '%s' "${url#http://github.com/}" ;;
    *)                      printf '' ;;
  esac
}

FORK_REPO="${FORK_REPO:-$(parse_owner_repo "$(git remote get-url origin 2>/dev/null || true)")}"
[ -n "$FORK_REPO" ] || die "无法从 origin remote URL 解析 owner/repo（可设 FORK_REPO=owner/repo）"

PARENT_REPO="${PARENT_REPO:-}"
[ -n "$PARENT_REPO" ] || PARENT_REPO="$(parse_owner_repo "$(git remote get-url upstream 2>/dev/null || true)")"
[ -n "$PARENT_REPO" ] || PARENT_REPO="$(gh api "repos/$FORK_REPO" --jq '.parent.full_name // empty' 2>/dev/null || true)"
[ -n "$PARENT_REPO" ] || die "无法确定上游 parent（可设 PARENT_REPO=owner/repo）"

BASE_BRANCH="${BASE_BRANCH:-main}"
CUR_BRANCH="$(git rev-parse --abbrev-ref HEAD)"

printf 'fork     : %s\n' "$FORK_REPO"
printf 'parent   : %s\n' "$PARENT_REPO"
printf '基线分支 : %s\n' "$BASE_BRANCH"
printf '当前分支 : %s\n' "$CUR_BRANCH"
printf 'go       : %s (%s)\n' "$GO_BIN" "$("$GO_BIN" version | awk '{print $3}')"

# ---------- 1. 工作区必须干净 ----------
sep "1/6 校验工作区"
if [ -n "$(git status --porcelain)" ]; then
  echo "工作区不干净，请先提交或 stash 后再同步:" >&2
  git status --short >&2
  exit 1
fi
echo "工作区干净 ✓"

# ---------- 2. 同步远程 fork 默认分支 ----------
# 注意: 必须带 <owner/repo> 参数才同步 GitHub 上的 fork；
#       不带参数的 `gh repo sync` 只动本地仓库（fetch + reset 本地默认分支）。
sep "2/6 同步远程 fork（gh repo sync $FORK_REPO）"
gh repo sync "$FORK_REPO"
echo "远程 fork $BASE_BRANCH 已同步 ✓"

# ---------- 3. 本地基线分支 ff 更新 ----------
sep "3/6 更新本地 $BASE_BRANCH"
git checkout "$BASE_BRANCH"
if ! git pull --ff-only; then
  die "本地 $BASE_BRANCH 无法 fast-forward（可能已分叉），请人工处理"
fi
base_sha="$(git rev-parse --short HEAD)"
echo "本地 $BASE_BRANCH = $base_sha ✓"

# ---------- 4. 合并进当前分支 ----------
sep "4/6 合并 $BASE_BRANCH 进 $CUR_BRANCH"
if [ "$CUR_BRANCH" = "$BASE_BRANCH" ]; then
  echo "当前就在基线分支，跳过合并 ✓"
else
  git checkout "$CUR_BRANCH"
  if ! git merge "$BASE_BRANCH"; then
    echo >&2
    echo "合并存在冲突，已停下（不自动解决）。冲突文件:" >&2
    git diff --name-only --diff-filter=U >&2
    echo >&2
    echo "解决后执行: git add <files> && git commit" >&2
    exit 1
  fi
  echo "合并完成 ✓"
fi

# ---------- 5. 编译 + 测试 ----------
sep "5/6 编译与测试"
echo "--- go build ---"; "$GO_BIN" build ./...
echo "--- go vet ---";   "$GO_BIN" vet ./...
echo "--- go test ---";  "$GO_BIN" test ./...
echo "编译与测试全绿 ✓"

# ---------- 6. 合并后人工核对项 ----------
# 背景: 上游曾于 1.11.7 前后下线任务功能（删文件 + 移除面板路由），
#       而 go test 覆盖不到前端 JS，因此合并后必须核对前后端一致性。
sep "6/6 前端一致性核对"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
warn=0

if [ -f internal/panel/panel.go ] && [ -f internal/panel/app.js ] && [ -f internal/panel/index.html ]; then
  # 6.1 后端注册的 panel 路由 vs 前端 api() 调用
  grep -oE '"(GET|POST) /panel/api/[^"]*"' internal/panel/panel.go | sed 's/"//g' | sort -u > "$tmpdir/routes"
  grep -oE "api\('[a-zA-Z_/{}\$]+'" internal/panel/app.js | sed "s/api('//;s/'//" | sort -u > "$tmpdir/api"
  while read -r p; do
    [ -n "$p" ] || continue
    case "$p" in */) continue ;; esac   # 以 / 结尾的是前缀拼接（如 accounts/ + uid），跳过
    grep -q "/panel/api/$p" "$tmpdir/routes" || { echo "  ⚠ 前端调用无对应后端路由: $p"; warn=1; }
  done < "$tmpdir/api"
  echo "  · 前端 api() 调用与后端路由核对完成（$(wc -l < "$tmpdir/api") 条）"

  # 6.2 前端 $('id') 是否都能在 index.html 找到
  while read -r id; do
    [ -n "$id" ] || continue
    grep -q "id=\"$id\"" internal/panel/index.html || { echo "  ⚠ 前端引用了不存在的元素 id: $id"; warn=1; }
  done < <(grep -oE "\\\$\('[a-zA-Z_0-9-]+'\)" internal/panel/app.js | sed "s/\$('//;s/')//" | sort -u)
  echo "  · 元素 id 覆盖核对完成"

  # 6.3 重复 JS 函数 / 顶层声明（自动合并常见的语义冲突）
  dups="$(grep -oE "^(async )?function [a-zA-Z_0-9]+" internal/panel/app.js | awk '{print $NF}' | sort | uniq -d)"
  [ -z "$dups" ] || { echo "  ⚠ 重复的 JS 函数声明:"; printf '%s\n' "$dups" | sed 's/^/    /'; warn=1; }
  dups="$(grep -oE "^(const|let|var) [a-zA-Z_0-9]+" internal/panel/app.js | awk '{print $2}' | sort | uniq -d)"
  [ -z "$dups" ] || { echo "  ⚠ 重复的 JS 顶层声明:"; printf '%s\n' "$dups" | sed 's/^/    /'; warn=1; }
  echo "  · 重复声明核对完成"

  # 6.4 JS 运行时冒烟（node 存在时真实执行 app.js 顶层求值）
  if command -v node >/dev/null 2>&1; then
    "$GO_BIN" test ./internal/panel/ -run 'TestAppJS' >/dev/null 2>&1 \
      && echo "  · JS 运行时冒烟通过（node $(node --version)）" \
      || { echo "  ⚠ JS 运行时冒烟失败，请单独执行: go test ./internal/panel/ -run TestAppJS -v"; warn=1; }
  else
    echo "  · 未找到 node，跳过 JS 运行时冒烟"
  fi
else
  echo "  （未找到 internal/panel 前端文件，跳过）"
fi

# ---------- 可选推送 ----------
if [ "$PUSH" = "1" ] && [ "$CUR_BRANCH" != "$BASE_BRANCH" ]; then
  sep "推送 $CUR_BRANCH → origin"
  git push origin "$CUR_BRANCH"
fi

sep "完成"
printf '%s\n' "$BASE_BRANCH = $base_sha" "$CUR_BRANCH = $(git rev-parse --short HEAD)"
[ "$warn" = "0" ] || echo "注意: 前端核对有告警（见上），请人工确认后再推送。"
