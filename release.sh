#!/usr/bin/env bash
# 本地一键发布：以当前 HEAD 的 7 位短 hash 作为版本，构建并创建 GitHub Release。
#
# 用法：
#   ./release.sh
#
# 前置：
#   - 当前分支已提交并推送，且与 upstream 完全一致
#   - gh CLI 已登录

set -euo pipefail
cd "$(dirname "$0")"

if [[ $# -ne 0 ]]; then
  echo "✗ 版本由当前 commit hash 自动生成，不再接受版本参数"
  exit 1
fi

command -v git >/dev/null || { echo "✗ 需要 git"; exit 1; }
command -v gh >/dev/null || { echo "✗ 需要 gh CLI"; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "✗ gh 未登录：先跑 gh auth login"; exit 1; }

if [[ -n "$(git status --porcelain)" ]]; then
  echo "✗ 工作区有未提交改动，请先提交后再发布"
  git status --short
  exit 1
fi

BRANCH="$(git branch --show-current)"
[[ -n "$BRANCH" ]] || { echo "✗ 当前处于 detached HEAD，无法发布"; exit 1; }
UPSTREAM="$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null || true)"
[[ -n "$UPSTREAM" ]] || { echo "✗ 分支 $BRANCH 没有 upstream，请先推送并设置 upstream"; exit 1; }

FULL_HASH="$(git rev-parse HEAD)"
REMOTE_HASH="$(git rev-parse '@{u}')"
if [[ "$FULL_HASH" != "$REMOTE_HASH" ]]; then
  echo "✗ 当前 HEAD 尚未与 $UPSTREAM 同步，请先推送并确认远端一致"
  echo "  本地：${FULL_HASH:0:7}"
  echo "  远端：${REMOTE_HASH:0:7}"
  exit 1
fi

VERSION="$(git rev-parse --short=7 HEAD)"
echo "  ◆ 分支：$BRANCH"
echo "  ◆ 发布版本：$VERSION"

if gh release view "$VERSION" >/dev/null 2>&1; then
  echo "✗ release $VERSION 已存在远端"
  exit 1
fi

echo "  ◆ 编译并打包 ..."
VERSION="$VERSION" ./build-mac.sh --zip

ARCH="$(uname -m)"
ZIP="Codex-Viewer-${VERSION}-${ARCH}.zip"
[[ -f "$ZIP" ]] || { echo "✗ 未生成 $ZIP"; exit 1; }

PREV_TAG="$(git describe --tags --abbrev=0 HEAD^ 2>/dev/null || true)"
if git show-ref --verify --quiet "refs/tags/$VERSION"; then
  TAG_HASH="$(git rev-list -n 1 "refs/tags/$VERSION")"
  [[ "$TAG_HASH" == "$FULL_HASH" ]] || { echo "✗ tag $VERSION 已指向其他提交"; exit 1; }
  echo "  ◆ tag $VERSION 已存在且指向当前提交"
else
  echo "  ◆ 创建 tag $VERSION ..."
  git tag -a "$VERSION" -m "Release $VERSION"
fi

echo "  ◆ 推送 tag 到 origin ..."
git push origin "refs/tags/$VERSION"

NOTES_FILE="$(mktemp)"
trap 'rm -f "$NOTES_FILE"' EXIT
{
  echo "## What's Changed"
  echo ""
  if [[ -n "$PREV_TAG" ]]; then
    git log --pretty=format:"- %s" "${PREV_TAG}..HEAD"
  else
    git log --pretty=format:"- %s" HEAD
  fi
  echo ""
  echo ""
  echo "## Install"
  echo ""
  echo "下载 \`${ZIP}\`，解压后双击 \`Codex Viewer.app\`。"
  echo "如果被 Gatekeeper 拦截：\`xattr -cr \"/Applications/Codex Viewer.app\"\`。"
  if [[ -n "$PREV_TAG" ]]; then
    echo ""
    echo "**Full Changelog**: https://github.com/HuLuca1998/codex-ui/compare/${PREV_TAG}...${VERSION}"
  fi
} > "$NOTES_FILE"

echo "  ◆ 创建 GitHub Release $VERSION ..."
gh release create "$VERSION" "$ZIP" \
  --verify-tag \
  --latest \
  --title "$VERSION" \
  --notes-file "$NOTES_FILE"

echo ""
echo "  ✓ $VERSION 已发布"
echo "  ✓ 用户的 App 后台轮询 30min 内会发现新版"
echo ""
