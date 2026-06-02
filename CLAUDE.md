# CLAUDE.md

## 重要规则：任何修改必须重新打包软件

本项目以 macOS 原生 App（`Codex Viewer.app`）作为发布形态，
前端 `index.html` / `tailwind.js` 通过 `//go:embed` 嵌入 Go 二进制，
Swift 外壳再把 Go 二进制打进 `.app`。**任何修改都不会被运行中的 App 看到**，
必须重新打包：

```bash
./build-mac.sh
```

适用范围（任一文件被改都要重打）：

- `main.go`、其它 `*.go`
- `index.html`、`tailwind.js`（embed 资源）
- `CodexViewer.swift`、`makeicon.swift`（原生外壳）
- `build-mac.sh` 本身

完成修改 → 跑打包脚本 → 把改动结果告诉用户之前，确认脚本退出码为 0。
仅靠 `go build`、`go run` 验证不够 —— 用户使用的是 `.app`，不是 `go run` 起的临时进程。

## 项目结构速览

- `main.go` — Go HTTP 服务：扫描 `~/.codex/sessions` 与 `~/.claude/projects`，
  SSE 推送实时事件，提供 `/api/run` 走 `claude --resume <sid> -p <msg>` 续聊
- `index.html` — 单页前端（深色 + Tailwind），两套渲染器（Codex / Claude）
- `CodexViewer.swift` — NSWindow + WKWebView 外壳，启动内嵌 Go 子进程；
  菜单栏每 60s 轮询 `/api/menubar`，驱动红点提醒，并对新 issue/PR、新评论发系统通知
  （`UserNotifications`，开关在设置→启动页 `notifyOnNewItems`，默认开）
- `build-mac.sh` — 一键打包到 `Codex Viewer.app`（加 `--zip` 产出 release artifact）
- `dev.sh` — 浏览器模式开发启动（仅依赖 Go）
- `release.sh` — 本地发布：调 `build-mac.sh --zip` + `gh release create`
- `update.go` — App 自动更新机制（每 30min 拉 GitHub Releases，按用户确认下载并原地替换）

## 开发回环

1. 改 `index.html` / `main.go`
2. 跑 `./build-mac.sh`（同时验证 Go 与 Swift 编译通过）
3. 让用户在 `.app` 里实测（本环境无 GUI 验证能力，见 memory）
