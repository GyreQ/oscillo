#!/bin/bash
# ai-trace 成品打包：一键产出 macOS + Windows 可分发成品
#
#   scripts/release.sh [VERSION]      # 默认 0.1.0
#
# 产物（release/v<VERSION>/）：
#   Oscillo-macos.dmg             macOS 通用版（Intel + Apple Silicon，内含 CLI）
#   Oscillo.app/                  同 DMG 内容，便于本机直接安装
#   Oscillo-windows-amd64.zip     Windows x64（Oscillo.exe + ai-trace.exe）
# 同时刷新 dist/ 下的 CLI 跨平台二进制（含 linux）。
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-0.1.0}"
OUT="$(pwd)/release/v${VERSION}"
APP="Oscillo"

echo "==> 0) 清理并创建输出目录 ${OUT}"
rm -rf "${OUT}"
mkdir -p "${OUT}"

echo "==> 1) 构建 CLI 跨平台二进制（dist/）"
make release VERSION="${VERSION}"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

echo "==> 2) 构建桌面端 macOS（arm64 + amd64 → universal）"
(cd desktop \
  && CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "${TMP}/oscillo-arm64" . \
  && CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "${TMP}/oscillo-amd64" .)
lipo -create "${TMP}/oscillo-arm64" "${TMP}/oscillo-amd64" -output "${TMP}/${APP}"

echo "==> 3) 组装 ${APP}.app（ad-hoc 签名）"
APPDIR="${OUT}/${APP}.app"
mkdir -p "${APPDIR}/Contents/MacOS" "${APPDIR}/Contents/Resources"
cp "${TMP}/${APP}" "${APPDIR}/Contents/MacOS/ai-trace"
cp desktop/build/Oscillo.icns "${APPDIR}/Contents/Resources/${APP}.icns"
sed "s/@VERSION@/${VERSION}/" desktop/build/Info.plist > "${APPDIR}/Contents/Info.plist"
# CLI universal 一并放进 bundle，追踪 agent 用：
#   /Applications/Oscillo.app/Contents/MacOS/ai-trace-cli run claude
lipo -create dist/ai-trace-darwin-arm64 dist/ai-trace-darwin-amd64 \
  -output "${APPDIR}/Contents/MacOS/ai-trace-cli"
codesign --force --deep --sign - "${APPDIR}"
codesign --verify --deep --strict "${APPDIR}"

echo "==> 4) 制作 macOS DMG"
DMG="${TMP}/dmg"
mkdir -p "${DMG}"
cp -R "${APPDIR}" "${DMG}/"
ln -s /Applications "${DMG}/Applications"
cat > "${DMG}/使用说明.txt" <<'EOF'
Oscillo — 本地 AI Agent 追踪
============================

【安装】把 Oscillo.app 拖进右侧 Applications 文件夹。

【首次打开】本应用未签名未公证，若提示「无法验证开发者」：
  系统设置 → 隐私与安全性 → 点「仍要打开」
（或终端执行：xattr -dr com.apple.quarantine /Applications/Oscillo.app）

【追踪 agent 请求】
  启动即追踪：  ai-trace run claude
                ai-trace run codex
  命令行工具已内置在应用内：
    alias ai-trace="/Applications/Oscillo.app/Contents/MacOS/ai-trace-cli"
  其它命令：ai-trace doctor（诊断）、ai-trace export（导出报告）

【日常使用】菜单栏托盘常驻：打开窗口 / Capture 系统代理开关 / 退出。
  不支持 base_url 的客户端走托盘的 Capture 开关（首次需信任 CA，有引导）。
  Dashboard 地址：http://127.0.0.1:8787

【数据】一切数据只保存在本地 ~/.ai-trace/
EOF
hdiutil create -volname "Oscillo ${VERSION}" -srcfolder "${DMG}" -ov -format UDZO \
  -o "${OUT}/${APP}-macos.dmg" >/dev/null

echo "==> 5) 构建 Windows x64（GUI + CLI）并打包 zip"
WINDIR="${OUT}/${APP}-windows-amd64"
mkdir -p "${WINDIR}"
(cd desktop \
  && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
     -ldflags "-s -w -H=windowsgui -X main.version=${VERSION}" -o "${WINDIR}/${APP}.exe" .)
cp dist/ai-trace-windows-amd64.exe "${WINDIR}/ai-trace.exe"
{
  printf '\xEF\xBB\xBF'   # UTF-8 BOM，老记事本不乱码
  cat <<'EOF'
Oscillo — 本地 AI Agent 追踪
============================

【运行要求】
  Windows 10 (1803+) 或 Windows 11，x64。
  WebView2 运行时（Win10/11 一般自带；若启动提示缺少 WebView2，
  到 https://developer.microsoft.com/microsoft-edge/webview2/ 安装）。

【开始使用】
  1. 解压到任意目录
  2. 双击 Oscillo.exe 运行（窗口 + 托盘常驻；关窗不退出，从托盘菜单退出）
  3. 若 SmartScreen 弹「已保护你的电脑」：点「更多信息 → 仍要运行」（本软件未签名）

【追踪 agent 请求】
  支持自定义 base_url 的客户端（Claude Code / Codex 等）：
    ai-trace run claude
    ai-trace run codex
  ai-trace.exe 与 Oscillo.exe 同目录，在终端直接运行；
  其它命令：ai-trace doctor（诊断）、ai-trace export --out report.json（导出）。

【MITM 兜底】不支持 base_url 的客户端：托盘菜单「Capture」开启系统代理，
  首次需信任 CA，应用内有引导。

【数据】全部保存在本地 %USERPROFILE%\.ai-trace\
EOF
} > "${WINDIR}/README.txt"
(cd "${OUT}" && zip -qry "${APP}-windows-amd64.zip" "${APP}-windows-amd64")
rm -rf "${WINDIR}"

echo "==> 完成 ${VERSION}"
ls -lh "${OUT}"
