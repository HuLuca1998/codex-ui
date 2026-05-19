#!/usr/bin/env bash
# 把 Codex Viewer 打包成真正的原生 macOS 应用（Codex Viewer.app）。
#   · 原生 NSWindow + WKWebView 外壳（系统红绿灯 / Dock 图标 / Cmd+Q）
#   · 内置 Go 服务作为子进程，离线自包含（Tailwind 已本地化）
# 依赖：Go、Swift 工具链（Xcode Command Line Tools）
set -euo pipefail
cd "$(dirname "$0")"

APP="Codex Viewer.app"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

command -v go      >/dev/null || { echo "✗ 需要 Go";      exit 1; }
command -v swiftc  >/dev/null || { echo "✗ 需要 Swift（请安装 Xcode Command Line Tools: xcode-select --install）"; exit 1; }

echo "  ◆ 编译后端服务 (Go) ..."
go build -ldflags "-s -w" -o "$TMP/codex-ui" .

echo "  ◆ 编译原生窗口 (Swift / WKWebView) ..."
swiftc -O -o "$TMP/CodexViewer" CodexViewer.swift -framework Cocoa -framework WebKit

echo "  ◆ 渲染应用图标 ..."
swiftc -O -o "$TMP/mkicon" makeicon.swift -framework Cocoa
"$TMP/mkicon" "$TMP/icon.png"
ICONSET="$TMP/AppIcon.iconset"; mkdir -p "$ICONSET"
for s in 16 32 128 256 512; do
  s2=$((s * 2))
  sips -z $s  $s  "$TMP/icon.png" --out "$ICONSET/icon_${s}x${s}.png"      >/dev/null
  sips -z $s2 $s2 "$TMP/icon.png" --out "$ICONSET/icon_${s}x${s}@2x.png"   >/dev/null
done

echo "  ◆ 组装 $APP ..."
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$TMP/CodexViewer" "$APP/Contents/MacOS/CodexViewer"
cp "$TMP/codex-ui"    "$APP/Contents/Resources/codex-ui"
chmod +x "$APP/Contents/MacOS/CodexViewer" "$APP/Contents/Resources/codex-ui"
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"

cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Codex Viewer</string>
  <key>CFBundleDisplayName</key><string>Codex Viewer</string>
  <key>CFBundleIdentifier</key><string>com.local.codexviewer</string>
  <key>CFBundleVersion</key><string>1.0</string>
  <key>CFBundleShortVersionString</key><string>1.0</string>
  <key>CFBundleExecutable</key><string>CodexViewer</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSAppTransportSecurity</key>
  <dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict>
</plist>
PLIST

# 临时签名 + 清除隔离属性，避免首次打开被 Gatekeeper 拦截
codesign --force --deep --sign - "$APP" >/dev/null 2>&1 || true
xattr -cr "$APP" 2>/dev/null || true

echo ""
echo "  ✓ 已生成「${APP}」"
echo "  ✓ 双击运行；可拖入「应用程序」文件夹或 Dock 常驻。"
echo ""
