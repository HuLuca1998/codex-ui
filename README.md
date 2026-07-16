# Session Viewer — Codex & Claude

实时观测 **Codex**（`~/.codex/sessions`）与 **Claude Code**（`~/.claude/projects`）
会话的查看器 —— 深色、实时、自包含的 AI 工作台界面。

自动监听两个会话目录，新会话 / 新消息秒级浮现，按消息类型分别渲染（用户输入 /
模型回复 / 推理 / 命令执行 / 补丁 diff / 工具调用 / 网络搜索 / 图像生成 / 审查结论 …），
重点突出输入与输出。两套日志格式各有渲染器，按来源自动分发。

> **状态**：个人项目，仅 macOS（Apple Silicon）。未做代码签名/公证 —— 自用没问题，
> 想分发给陌生人请自行签名或源码自打。

## 安装

### 方式一：下载预编译 App（推荐）

到 [Releases](https://github.com/HuLuca1998/codex-ui/releases) 页面下载最新 `Codex-Viewer-<commit>-arm64.zip`，
解压后双击 `Codex Viewer.app`。首次启动若被 Gatekeeper 拦截：

```bash
xattr -cr "/Applications/Codex Viewer.app"
```

或在 系统设置 → 隐私与安全性 → 「仍要打开」。

App 启动后会每 30 分钟自动检查更新；新版本可在「设置 → 关于」一键应用。

### 方式二：源码打包

```bash
./build-mac.sh
```

依赖：Go、Swift 工具链（`xcode-select --install`）。
生成的 `Codex Viewer.app` 可拖入「应用程序」或 Dock 常驻。

### 方式三：浏览器模式（开发）

```bash
./dev.sh
```

编译并启动 HTTP 服务，自动在默认浏览器打开。依赖：仅 Go。

可选参数 / 环境变量：

| 项 | 说明 |
|----|------|
| `CODEX_SESSIONS=/path` | 指定 Codex 会话目录（默认 `~/.codex/sessions`） |
| `CLAUDE_PROJECTS=/path` | 指定 Claude 会话目录（默认 `~/.claude/projects`） |
| `PORT=8000` | 指定起始端口（默认 7800，被占用则顺延） |
| `NO_OPEN=1` | 不自动打开浏览器 |
| `CODEXUI_TOKEN=xxx` | 固定 token（默认每次启动随机生成） |

> 任一目录不存在会自动跳过；两个都在则同时监控。

## 界面

- **左侧**：会话列表。来源筛选（全部 / Codex / Claude）；选 Claude 时额外出现项目下拉过滤；
  时间过滤（今天 / 本周 / 本月 / 全部，默认今天）+ 关键词搜索。
  进行中的会话实时置顶并跳动，带来源徽章。
  子代理（subagent）按父会话折叠成树。
- **中间**：对话时间线 —— 聊天流为主，工具调用 / 推理 / 系统事件折叠成卡片，点击展开；
  每条可查看原始 JSON。
- **右下角**：悬浮 token 用量，随事件流实时更新。
- **底部**：悬浮控制台 —— 在当前对话中查找、按类别（对话 / 思考 / 工具 / 系统）过滤、
  切换自动跟随；Claude 会话还能直接续聊。

## 局域网 / 手机访问

App 自带 LAN 分享：左下角 ⇪ 按钮 → 复制带 token 的 URL → 同 Wi-Fi 手机/电脑打开。
URL 形如 `http://192.168.x.x:7800/?t=xxxxx`。

**会话深链：** 右键任一会话 → 「🔗 分享会话」生成带 `?type=&session=` 的链接，
对方打开直接定位到该会话。

**移动端适配：** 自动按 768px 断点切换布局 —— 侧栏改为抽屉、长按列表项=右键菜单。

## 自动更新

App 后台每 30 分钟轮询 GitHub Releases；发现新版本会在「设置 → 关于」高亮，
点击「下载」→ 完成后「重启应用」即原地替换。

## 发布（维护者）

本地一键发布（不依赖 CI）：

```bash
git push origin main
./release.sh
```

脚本读取当前 `HEAD` 的 7 位短 commit hash 作为版本，例如 `53a82fa`。它会先确认
工作区干净且当前分支与 upstream 一致，再运行 `build-mac.sh --zip`，创建并推送同名
Git Tag，最后通过 `gh release create` 上传产物并将该 Release 标记为 Latest。

## 项目结构

| 文件 | 说明 |
|------|------|
| `main.go` | Go 服务：会话扫描、轮询、SSE 实时推送、HTTP 接口 |
| `update.go` | 自动更新机制（GitHub Releases 轮询 + 原地替换） |
| `index.html` | 单页前端（内嵌进二进制） |
| `tailwind.js` | 本地化的 Tailwind（离线自包含，内嵌进二进制） |
| `CodexViewer.swift` | 原生 macOS 窗口外壳（WKWebView） |
| `makeicon.swift` | 应用图标渲染 |
| `build-mac.sh` | 打包脚本（加 `--zip` 产出 release artifact） |
| `dev.sh` | 浏览器模式开发启动 |
| `release.sh` | 本地发布（build-mac.sh --zip + gh release create） |
| `mobile-mockup.html` | 移动端设计稿预览（独立 HTML，不进生产构建） |

> 修改 `index.html` / `main.go` 后需重新跑 `./build-mac.sh`（HTML 在编译期内嵌进二进制）。

## License

MIT
