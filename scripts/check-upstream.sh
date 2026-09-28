#!/usr/bin/env bash
# check-upstream.sh — 检查本 fork 是否落后于上游 parent（只读：不 fetch、不提交、不推送）
#
# 用法:
#   ./scripts/check-upstream.sh
#
# 可覆盖的环境变量:
#   FORK_REPO    默认从 origin remote URL 解析（如 rangdl/workbuddy2api-panel）
#   PARENT_REPO  默认取 upstream remote URL，其次取 fork 的 parent 字段
#   BASE_BRANCH  默认 main
#
# 输出: 上游领先/落后提交数、上游新增提交列表、涉及文件、
#       与当前分支改动重叠的文件（合并冲突风险）、试合并预演结论。
#
# 退出码: 0 = 检查完成（无论是否落后）；非 0 = 检查本身失败。
set -euo pipefail

export PATH="$HOME/.local/bin:$PATH"

sep() { printf '\n=== %s ===\n' "$*"; }

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || true)"
[ -n "$repo_root" ] || { echo "错误: 当前目录不是 git 仓库" >&2; exit 1; }
cd "$repo_root"

# ---------- fork / parent 探测 ----------
# 坑: 同时存在 origin(fork) 与 upstream(parent) 两个 remote 时，
#     `gh repo view` 会解析到 upstream（返回 parent 而不是 fork），
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
[ -n "$FORK_REPO" ] || { echo "错误: 无法从 origin remote URL 解析 owner/repo（可设 FORK_REPO=owner/repo）" >&2; exit 1; }

PARENT_REPO="${PARENT_REPO:-}"
[ -n "$PARENT_REPO" ] || PARENT_REPO="$(parse_owner_repo "$(git remote get-url upstream 2>/dev/null || true)")"
[ -n "$PARENT_REPO" ] || PARENT_REPO="$(gh api "repos/$FORK_REPO" --jq '.parent.full_name // empty' 2>/dev/null || true)"
[ -n "$PARENT_REPO" ] || { echo "错误: 无法确定上游 parent（可设 PARENT_REPO=owner/repo）" >&2; exit 1; }

BASE_BRANCH="${BASE_BRANCH:-main}"
FORK_OWNER="${FORK_REPO%%/*}"
PARENT_OWNER="${PARENT_REPO%%/*}"
CUR_BRANCH="$(git rev-parse --abbrev-ref HEAD)"

printf 'fork     : %s\n' "$FORK_REPO"
printf 'parent   : %s\n' "$PARENT_REPO"
printf '基线分支 : %s\n' "$BASE_BRANCH"
printf '当前分支 : %s\n' "$CUR_BRANCH"

# ---------- compare API ----------
# 方向语义: GET /repos/{parent}/compare/{base}...{head} 的 ahead_by = head 比 base 多的提交数
#   {fork}:main...{parent}:main → ahead_by = 上游比 fork 多几个（= fork 缺几个）
#                               → behind_by = fork 比上游多几个
# 坑: 跨 fork 比较必须同 network；ref 必须写 owner:branch，
#     且 head 参数不能带 repo 名（写成 owner:repo:branch 会 404）。
# 用 gh api 自带的 --jq（gojq）过滤，避免依赖外部 jq。
cmp_get() {
  gh api "repos/$PARENT_REPO/compare/$FORK_OWNER:$BASE_BRANCH...$PARENT_OWNER:$BASE_BRANCH" --jq "$1"
}

upstream_ahead=""
fork_ahead=""
cmp_ok=0
if counts="$(cmp_get '"\(.ahead_by) \(.behind_by)"' 2>/dev/null)" && [ -n "$counts" ]; then
  cmp_ok=1
  upstream_ahead="${counts%% *}"
  fork_ahead="${counts##* }"
else
  # 降级: 用本地 ref（需已 git fetch upstream）
  if git rev-parse --verify -q "upstream/$BASE_BRANCH" >/dev/null \
     && git rev-parse --verify -q "origin/$BASE_BRANCH" >/dev/null; then
    upstream_ahead="$(git rev-list --count "origin/$BASE_BRANCH..upstream/$BASE_BRANCH")"
    fork_ahead="$(git rev-list --count "upstream/$BASE_BRANCH..origin/$BASE_BRANCH")"
    echo "（compare API 不可用，已降级为本地 ref 比较）"
  else
    echo "错误: compare API 不可用，且本地缺少 origin/$BASE_BRANCH 或 upstream/$BASE_BRANCH" >&2
    echo "      可先执行: git fetch upstream && git fetch origin" >&2
    exit 1
  fi
fi

sep "上游领先 / 落后"
printf '上游领先（fork 缺）: %s 个提交\n' "$upstream_ahead"
printf 'fork 领先（上游缺）: %s 个提交\n' "$fork_ahead"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

sep "上游新增提交"
if [ "$cmp_ok" = 1 ]; then
  cmp_get '.commits[] | "\(.sha[0:7])  \(.commit.author.date[0:10])  \(.commit.message | split("\n")[0])"'
else
  git log --date=short --pretty='%h  %ad  %s' "origin/$BASE_BRANCH..upstream/$BASE_BRANCH"
fi

sep "上游新增涉及文件"
if [ "$cmp_ok" = 1 ]; then
  cmp_get '.files[].filename' | sort -u > "$tmpdir/upstream_files"
else
  git diff --name-only "origin/$BASE_BRANCH..upstream/$BASE_BRANCH" | sort -u > "$tmpdir/upstream_files"
fi
cat "$tmpdir/upstream_files"

sep "与当前分支改动重叠的文件（合并冲突风险）"
if git rev-parse --verify -q "origin/$BASE_BRANCH" >/dev/null; then
  mb="$(git merge-base HEAD "origin/$BASE_BRANCH" 2>/dev/null || true)"
  if [ -n "$mb" ]; then
    git diff --name-only "$mb...HEAD" | sort -u > "$tmpdir/branch_files"
  else
    : > "$tmpdir/branch_files"
  fi
  overlap="$(comm -12 "$tmpdir/upstream_files" "$tmpdir/branch_files" || true)"
  if [ -n "$overlap" ]; then
    printf '%s\n' "$overlap"
  else
    echo "（无重叠）"
  fi
else
  echo "（本地无 origin/$BASE_BRANCH，跳过）"
fi

sep "试合并预演（只读，不改动工作区）"
if git rev-parse --verify -q "upstream/$BASE_BRANCH" >/dev/null; then
  if out="$(git merge-tree --write-tree --name-only HEAD "upstream/$BASE_BRANCH" 2>&1)"; then
    echo "干净合并（无冲突）"
  else
    echo "存在冲突，需手工解决:"
    printf '%s\n' "$out" | awk 'NR==1{next} /^$/{exit} {print}' | sed 's/^/  冲突文件: /'
    printf '%s\n' "$out" | grep '^CONFLICT' | sed 's/^/  /' || true
  fi
else
  echo "（本地无 upstream/$BASE_BRANCH，跳过；可先 git fetch upstream）"
fi

sep "结论"
if [ "$upstream_ahead" = "0" ]; then
  echo "已是最新，无需同步。"
else
  echo "落后上游 $upstream_ahead 个提交。执行 ./scripts/sync-upstream.sh 完成同步 + 合并 + 验证。"
fi
