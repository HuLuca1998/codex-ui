#!/usr/bin/env bash
# 本地一键发布：自动递增版本 → 编译 → zip → gh release create
#
# 用法：
#   ./release.sh                # patch 自增（默认）：v0.1.2 → v0.1.3
#   ./release.sh patch          # 同上
#   ./release.sh minor          # minor 自增：v0.1.2 → v0.2.0
#   ./release.sh major          # major 自增：v0.1.2 → v1.0.0
#   ./release.sh v1.2.3         # 显式指定版本号
#   VERSION=v1.2.3 ./release.sh # 同上（环境变量）
#
# 前置：
#   - gh CLI 已登录
#   - 工作区无未提交改动（不强制；脚本会询问）

set -euo pipefail
cd "$(dirname "$0")"

# ── 版本号解析 ────────────────────────────────────────────────
ARG="${1:-${VERSION:-}}"
LAST_TAG="$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)"
echo "  ◆ 最近 tag：$LAST_TAG"

bump_version(){
  # 输入：上一个 tag（vX.Y.Z）+ 自增级别（major|minor|patch）
  # 输出：vX.Y.Z 形式的新版本
  local prev="${1#v}" level="$2"
  IFS='.' read -r X Y Z <<<"${prev:-0.0.0}"
  X="${X:-0}"; Y="${Y:-0}"; Z="${Z:-0}"
  # 去掉非数字尾巴（防 v0.1.0-3-gabcdef 这种）
  Z="${Z%%[!0-9]*}"; Y="${Y%%[!0-9]*}"; X="${X%%[!0-9]*}"
  case "$level" in
    major) X=$((X+1)); Y=0; Z=0 ;;
    minor) Y=$((Y+1)); Z=0 ;;
    patch|*) Z=$((Z+1)) ;;
  esac
  echo "v${X}.${Y}.${Z}"
}

case "$ARG" in
  ""|patch)         VERSION=$(bump_version "$LAST_TAG" patch) ;;
  minor)            VERSION=$(bump_version "$LAST_TAG" minor) ;;
  major)            VERSION=$(bump_version "$LAST_TAG" major) ;;
  v[0-9]*|[0-9]*)   VERSION="${ARG#v}"; VERSION="v${VERSION}" ;;
  *)                echo "✗ 无效参数：$ARG（用 patch / minor / major / vX.Y.Z）"; exit 1 ;;
esac
echo "  ◆ 发布版本：$VERSION"

# ── 前置检查 ────────────────────────────────────────────────
command -v gh >/dev/null || { echo "✗ 需要 gh CLI"; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "✗ gh 未登录：先跑 gh auth login"; exit 1; }

if [[ -n "$(git status --porcelain)" ]]; then
  echo "⚠ 工作区有未提交改动"
  read -r -p "  继续发布？[y/N] " yn
  [[ "$yn" =~ ^[Yy]$ ]] || exit 1
fi

# 远端是否已有同名 release
if gh release view "$VERSION" >/dev/null 2>&1; then
  echo "✗ release $VERSION 已存在远端"
  exit 1
fi

# ── 构建 + 打包 ─────────────────────────────────────────────
echo "  ◆ 编译并打包 ..."
VERSION="$VERSION" ./build-mac.sh --zip

ARCH="$(uname -m)"
SHORT_VERSION="${VERSION#v}"
ZIP="Codex-Viewer-${SHORT_VERSION}-${ARCH}.zip"
[[ -f "$ZIP" ]] || { echo "✗ 未生成 $ZIP"; exit 1; }

# ── 打 tag（若不存在）并 push ─────────────────────────────────
if ! git rev-parse "$VERSION" >/dev/null 2>&1; then
  echo "  ◆ 打 tag $VERSION ..."
  git tag -a "$VERSION" -m "Release $VERSION"
fi
echo "  ◆ push tag 到 origin ..."
git push origin "$VERSION" 2>&1 | tail -3 || true

# ── 生成 release notes（上一 tag → HEAD 的 commit log） ──────
PREV_TAG="$(git describe --tags --abbrev=0 "${VERSION}^" 2>/dev/null || true)"
NOTES_FILE="$(mktemp)"
trap 'rm -f "$NOTES_FILE"' EXIT
{
  echo "## What's Changed"
  echo ""
  if [[ -n "$PREV_TAG" ]]; then
    git log --pretty=format:"- %s" "${PREV_TAG}..${VERSION}"
  else
    git log --pretty=format:"- %s" "$VERSION"
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

# ── 创建 release + 上传 artifact ─────────────────────────────
echo "  ◆ gh release create $VERSION ..."
gh release create "$VERSION" "$ZIP" \
  --title "$VERSION" \
  --notes-file "$NOTES_FILE"

echo ""
echo "  ✓ $VERSION 已发布"
echo "  ✓ 用户的 App 后台轮询 30min 内会发现新版"
echo ""
