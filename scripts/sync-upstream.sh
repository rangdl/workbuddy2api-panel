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
# 可覆盖的环境变量:
#   FORK_REPO / PARENT_REPO / BASE_BRANCH / GO_BIN
#
# 退出码: 0 = 成功；非 0 = 任一步骤失败（含合并冲突）。
set -euo pipefail

export PATH="$HOME/.local/bin:$PATH"

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
