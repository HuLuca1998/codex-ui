# Codex Viewer 多模块设置系统 — 设计文档

日期：2026-05-20
状态：已确认，待实现

## 1. 背景与目标

**现状**：设置面板只有单页「设置 · 仓库关联」，配置文件 `~/.codex-viewer.json`
只有一个 `repos` 字段。大量运行参数硬编码在代码里：

- 扫描间隔 `800ms`（main.go 扫描循环）
- issue 刷新间隔 `3 分钟`（main.go）
- 菜单栏最多显示 `20` 条（CodexViewer.swift `prefix(20)`）
- issue 查询写死 `--assignee @me --state open --limit 50`
- 浏览器优先 Chrome、终端优先 iTerm（`openExternal` / `launchITerm`）
- claude resume 写死 `--permission-mode bypassPermissions --allow-dangerously-skip-permissions`
- Claude 上下文窗口写死 `1000000`
- codex 续聊 / 发消息、issue「详情」按钮命令均硬编码
- 数据源目录只能靠环境变量 / 启动参数

**目标**：把设置重构成 8 个模块的侧边栏式面板，将上述硬编码项变为可配置；
配置文件升级为嵌套结构，并对老配置向后兼容。

## 2. 范围

8 个模块：

1. GitHub 账号
2. Codex
3. Claude
4. Issue 过滤
5. 通用 · 集成
6. 性能 · 扫描
7. 启动
8. 关于 · 维护

## 3. 非目标（YAGNI）

- **不做**多套命名过滤方案 / 预设 —— 已确认单套全局过滤足够。
- **不做** raw `--search` 高级查询框 —— 结构化下拉足够。
- **不做**每仓库独立过滤 —— 全局一套作用于所有关联仓库。
- 不做 GitHub Enterprise 多 host。
- 不做主题切换，保持暗色。

## 4. 配置结构

`~/.codex-viewer.json` 从扁平 `{repos}` 升级为嵌套。`loadConfig` 增加
`withDefaults()`：读入后对所有缺失字段填默认值，因此现有用户的 `repos` 不丢失，
其余模块自动取默认。

```go
type Config struct {
    Repos   []RepoMap     `json:"repos"`   // 保留：仓库 ↔ 本地目录 ↔ 主分支
    Codex   AgentConfig   `json:"codex"`
    Claude  ClaudeConfig  `json:"claude"`
    Issue   IssueConfig   `json:"issue"`
    General GeneralConfig `json:"general"`
    Perf    PerfConfig    `json:"perf"`
    Startup StartupConfig `json:"startup"`
}

type AgentConfig struct {   // Codex
    Enabled      bool   `json:"enabled"`      // 默认 true
    SessionsPath string `json:"sessionsPath"` // 默认 ~/.codex/sessions
    ResumeCmd    string `json:"resumeCmd"`    // 默认 "codex resume {sid}"
    SendCmd      string `json:"sendCmd"`      // 默认 "codex exec resume {sid} {input}"
}

type ClaudeConfig struct {
    Enabled         bool     `json:"enabled"`         // 默认 true
    ProjectsPath    string   `json:"projectsPath"`    // 默认 ~/.claude/projects
    WatchedProjects []string `json:"watchedProjects"` // 监控的项目子目录；空 = 全部
    ResumeCmd       string   `json:"resumeCmd"`       // 默认含 bypassPermissions flag
    ContextWindow   int      `json:"contextWindow"`   // 默认 1000000
}

type IssueConfig struct {
    Assignee       string   `json:"assignee"`       // 默认 "@me"，空 = 不限
    State          string   `json:"state"`          // open|closed|all，默认 open
    Labels         []string `json:"labels"`         // 标签过滤，默认空
    Limit          int      `json:"limit"`          // 默认 50
    RefreshMinutes int      `json:"refreshMinutes"` // 默认 3
    MenuMax        int      `json:"menuMax"`        // 菜单栏最多显示，默认 20
    ShowInMenu     bool     `json:"showInMenu"`     // 默认 true
    DetailCmd      string   `json:"detailCmd"`      // 「详情」按钮命令模版
}

type GeneralConfig struct {
    Browser      string `json:"browser"`      // chrome|safari|default|custom，默认 chrome
    BrowserPath  string `json:"browserPath"`  // custom 时用
    Terminal     string `json:"terminal"`     // iterm|terminal|warp|custom，默认 iterm
    TerminalPath string `json:"terminalPath"` // custom 时用
}

type PerfConfig struct {
    ScanIntervalMs  int `json:"scanIntervalMs"`  // 默认 800
    DetailBudget    int `json:"detailBudget"`    // 大会话截断字节预算，默认 = 现有常量
    DetailMaxN      int `json:"detailMaxN"`      // 大会话截断条数，默认 = 现有常量
    ActiveWindowSec int `json:"activeWindowSec"` // "活跃" 判定时间窗，默认 = 现有值
}

type StartupConfig struct {
    LaunchAtLogin     bool   `json:"launchAtLogin"`     // 默认 false
    OpenWindowOnLaunch bool  `json:"openWindowOnLaunch"`// 默认 true
    OnWindowClose     string `json:"onWindowClose"`     // background|quit，默认 background
}
```

GitHub 账号、关于 · 维护 两个模块**不存配置**：前者读 `gh` 实时状态，
后者是动作按钮。

## 5. 八个模块字段清单

| 模块 | 字段 / 动作 | 默认值 |
|---|---|---|
| **GitHub 账号** | 显示 `gh auth status`（账号 / host / 当前活跃）；按钮：登录新账号、切换活跃账号、测试连接 | 实时读取 |
| **Codex** | 启用监控开关 · 数据源目录 · 续聊命令模版 · 后台发消息命令模版 | `~/.codex/sessions`；`codex resume {sid}`；`codex exec resume {sid} {input}` |
| **Claude** | 启用监控开关 · 数据源目录 · 监控哪些项目（多选）· 续聊命令模版 · 上下文窗口大小 | `~/.claude/projects`；空 = 全部；`claude --resume {sid} --permission-mode bypassPermissions --allow-dangerously-skip-permissions`；`1000000` |
| **Issue 过滤** | assignee · state · labels · limit · 刷新间隔 · 菜单栏最多显示 · 是否在菜单栏显示 · 「详情」命令模版 | `@me` · open · 空 · 50 · 3 分钟 · 20 · 开 · `claude --permission-mode bypassPermissions --allow-dangerously-skip-permissions "/issue info {number}"` |
| **通用 · 集成** | 浏览器（Chrome / Safari / 默认 / 自定义）· 终端（iTerm2 / Terminal / Warp / 自定义） | Chrome · iTerm2 |
| **性能 · 扫描** | 扫描间隔 · 大会话截断阈值（budget / maxN）· 「活跃」判定时间窗 | 800ms · 现有常量 · 现有值 |
| **启动** | 开机自启动 · 启动时开窗 / 仅后台 · 关窗行为（转后台 / 退出） | 关 · 开窗 · 转后台 |
| **关于 · 维护** | 导出配置 · 导入配置 · 立即重新扫描 · 配置文件路径 · 版本号 | — |

### 命令模版与占位符

模版支持占位符：`{sid}` `{input}` `{number}` `{repo}` `{path}`。

- 后端替换占位符时**自动 shell 转义**（单引号包裹），防止命令注入。
- `cd 到目标目录` 由后端自动加在命令前，模版只写命令本身。
- 保存前校验必需占位符：`SendCmd` 必须含 `{input}` 与 `{sid}`；
  `DetailCmd` 必须含 `{number}`；`ResumeCmd` 必须含 `{sid}`。

## 6. 设置面板布局

侧边栏式（仿 macOS 系统设置）：左侧 8 个模块导航，右侧当前模块表单。

```
┌─ 设置 ───────────────────────────────┐
│ GitHub 账号 │                          │
│ Codex       │   【右侧：当前模块表单】    │
│ Claude      │                          │
│ Issue 过滤  │   assignee  [@me   ▾]    │
│ 通用        │   state     [open  ▾]    │
│ 性能        │   label     [________]   │
│ 启动        │   limit     [50      ]   │
│ 关于        │                          │
├─────────────────────────────────────┤
│ [状态信息]              [完成] [保存]   │
└─────────────────────────────────────┘
```

- 仓库关联（现有 `repoRows`）并入「Issue 过滤」模块或单列一节。
- 保存 = POST `/api/config` 整体写回。

## 7. 后端 / Swift 改动

### 新增 API
- `GET  /api/gh/status` —— 运行 `gh auth status`，解析账号列表。
- `POST /api/gh/login`  —— `launchITerm("gh auth login")`。
- `POST /api/gh/switch` —— `gh auth switch`。
- `GET  /api/claude/projects` —— 列 `~/.claude/projects` 子目录供多选。
- `POST /api/rescan`    —— 立即触发一次全量扫描。
- `GET  /api/config/export` / `POST /api/config/import` —— 导出 / 导入配置。

### 改造点
- 扫描循环每轮读 config：尊重 `Codex.Enabled` / `Claude.Enabled`、
  数据源路径、`Perf.ScanIntervalMs`、`Claude.WatchedProjects`。
- `refreshIssues` 按 `IssueConfig` 拼 `gh issue list` 参数；刷新间隔用
  `Issue.RefreshMinutes`。
- `resumeHandler` / `sendHandler` / `issueRunHandler` 改用命令模版。
- `openExternal` / `launchITerm` 读 `GeneralConfig` 选择浏览器 / 终端。
- `issuesHandler` 响应附带 `menuMax` / `showInMenu`，供 Swift 菜单栏使用。

### Swift 侧
- 菜单栏按 `showInMenu` / `menuMax` 控制 issue 显示。
- 开机自启动用 `SMAppService`（macOS 13+）。
- 关窗行为、启动时开窗按 `StartupConfig` 处理。

## 8. 生效方式

| 立即热生效 | 下一轮扫描生效 | Swift 立即应用 |
|---|---|---|
| issue 过滤、命令模版、浏览器 / 终端选择 | 监控开关、数据源路径、扫描间隔、监控项目 | 开机自启动、关窗行为 |

需 App 重启才彻底生效的项会在 UI 上标注。

## 9. 错误处理

- `gh` 未安装 / 未登录 → GitHub 模块显式提示，不崩溃。
- 目录路径无效 → 输入框标红 + 提示。
- 命令模版缺必需占位符 → 保存前拦截并提示。
- 导入配置 → 先 JSON 校验 + 字段白名单，失败不覆盖现有配置。
- 监控开关全关 → 提示「当前无数据源」。

## 10. 兼容性

- 老 `~/.codex-viewer.json`（仅 `repos`）经 `withDefaults()` 自动补全，
  无需迁移脚本。
- 数据源默认路径与现有环境变量 `CODEX_SESSIONS` / `CLAUDE_PROJECTS`
  逻辑保持兼容（配置为空时回退到环境变量 / 默认路径）。
