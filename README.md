# Session Viewer — Codex & Claude

实时观测 **Codex**（`~/.codex/sessions`）与 **Claude Code**（`~/.claude/projects`）
会话的查看器 —— 深色、实时、自包含的 AI 工作台界面。

自动监听两个会话目录，新会话 / 新消息秒级浮现，按消息类型分别渲染（用户输入 /
模型回复 / 推理 / 命令执行 / 补丁 diff / 工具调用 / 网络搜索 / 图像生成 / 审查结论 …），
重点突出输入与输出。两套日志格式各有渲染器，按来源自动分发。

## 两种运行方式

### 1) 原生 macOS App（推荐）

```bash
./打包macOS-App.sh
```

生成「Codex Viewer.app」—— 真正的原生窗口（系统红绿灯 / Dock 图标 / Cmd+Q），
内置 Go 服务，完全离线自包含。双击运行，可拖入「应用程序」或 Dock 常驻。

依赖：Go、Swift 工具链（`xcode-select --install`）。

### 2) 浏览器模式

```bash
./启动脚本.sh
```

编译并启动服务，自动在默认浏览器打开。依赖：仅 Go。

可选参数 / 环境变量：

| 项 | 说明 |
|----|------|
| `CODEX_SESSIONS=/path` | 指定 Codex 会话目录（默认 `~/.codex/sessions`） |
| `CLAUDE_PROJECTS=/path` | 指定 Claude 会话目录（默认 `~/.claude/projects`） |
| `PORT=8000` | 指定起始端口（默认 7800，被占用则顺延） |
| `NO_OPEN=1` | 不自动打开浏览器 |

> 任一目录不存在会自动跳过；两个都在则同时监控。

## 界面

- **左侧**：会话列表。来源筛选（全部 / Codex / Claude）；选 Claude 时额外出现项目下拉过滤；
  时间过滤（今天 / 本周 / 本月 / 全部，默认今天）+ 关键词搜索。进行中的会话实时置顶并跳动，带来源徽章。
- **中间**：对话时间线 —— 聊天流为主，工具调用 / 推理 / 系统事件折叠成卡片，点击展开；每条可查看原始 JSON。
- **右下角**：悬浮 token 用量，随事件流实时更新。
- **底部**：悬浮控制台 —— 在当前对话中查找、按类别（对话 / 思考 / 工具 / 系统）过滤、切换自动跟随。

## 结构

| 文件 | 说明 |
|------|------|
| `main.go` | Go 服务：会话目录扫描、轮询、SSE 实时推送、HTTP 接口 |
| `index.html` | 单页前端（内嵌进二进制） |
| `tailwind.js` | 本地化的 Tailwind（离线自包含，内嵌进二进制） |
| `CodexViewer.swift` | 原生 macOS 窗口外壳（WKWebView） |
| `makeicon.swift` | 应用图标渲染 |

> 修改 `index.html` 后需重新编译（HTML 在编译期内嵌进二进制）。
