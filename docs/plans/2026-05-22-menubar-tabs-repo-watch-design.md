# 菜单栏三标签页 + 仓库关注 — 设计文档

日期：2026-05-22

## 背景

菜单栏 `NSStatusItem` 目前只展示分配给我的 GitHub issue。需要扩展为三个标签页
（issue / PR / 活跃会话），并在设置里新增「仓库关注」能力，让用户选择哪些仓库参与
菜单栏的 issue / PR 汇总。

## 目标

1. 设置面板新增独立「仓库」模块，每个仓库带「关注」开关。
2. 菜单栏顶部用 `NSSegmentedControl` 切换三个标签页：
   - **issue**：关注仓库里分配给我的 issue（沿用现有逻辑）。
   - **PR**：关注仓库里「我相关」的 PR —— 我创建的 + 待我 review 的。
   - **活跃会话**：会话文件 mtime 在 60 秒内的会话；点击唤起主窗口并定位。

## 决策（已与用户确认）

- 仓库关注：新增独立「仓库」设置模块 + 每仓库「关注」开关。
- PR 范围：我相关的（`--author @me` + `review-requested:@me`）。
- 会话点击：打开主窗口并定位到该会话。
- 标签切换控件：菜单顶部 `NSSegmentedControl`。

## 后端 `main.go`

### 仓库关注字段

`RepoMap` 增加 `Watch bool`（`json:"watch"`）。用自定义 `UnmarshalJSON` 让缺省的
`watch` 字段视为 `true`，保证旧配置升级后不会丢失关注状态：

```go
func (r *RepoMap) UnmarshalJSON(data []byte) error {
    type alias RepoMap
    a := alias{Watch: true} // 缺省视为关注，兼容旧配置
    if err := json.Unmarshal(data, &a); err != nil {
        return err
    }
    *r = RepoMap(a)
    return nil
}
```

新增 `watchedRepos()` 助手：返回 `Watch==true` 且 `Repo` 非空的仓库。
`refreshIssues` 改为遍历 `watchedRepos()`。

### PR 拉取

```go
type PR struct {
    Repo, Title, URL, UpdatedAt, Author, Reason string
    Number  int
    Labels  []Label
    IsDraft bool
}
```

`ghPRs(repo)`：对单仓库跑两次 `gh pr list --json number,title,labels,updatedAt,url,author,isDraft`：
- `--author @me` → `Reason="author"`
- `--search "review-requested:@me"` → `Reason="review"`

按 `Number` 合并去重（`author` 优先）。缓存 `prs / prsAt / prsErr`，由现有 issue
刷新 goroutine 一并刷新（`refreshIssues` 后调用 `refreshPRs`）。

### `/api/menubar` 端点

一次返回菜单栏所需全部数据，Swift 侧一次 fetch、切标签零延迟：

```json
{
  "showInMenu": true, "menuMax": 20,
  "issues": [...], "issuesUpdated": 0, "issuesError": "",
  "prs": [...],    "prsUpdated": 0,    "prsError": "",
  "sessions": [...]
}
```

活跃会话项 `MenuSession`：`{id, source, title, project, mtime}`，由 `states`
过滤 `now - mtime < 60000` 得到，按 mtime 倒序。`project` 取 `cwd` 的 basename。

保留旧的 `/api/issues` 端点不动（向后兼容）。

## 原生外壳 `CodexViewer.swift`

- 新增 `enum MenuTab { issues, prs, sessions }`，实例变量 `selectedTab` 跨菜单开合保留。
- `menuNeedsUpdate`：fetch `/api/menubar` → 缓存 → 构建菜单：
  1. 第一项：自定义视图，内嵌 `NSSegmentedControl`（三段带计数徽标）。
     段变化 → 设置 `selectedTab` → 重建菜单下半部分。
  2. 按 `selectedTab` 渲染对应行。
- **issue 行**：沿用 `IssueRowView`。
- **PR 行**：新增 `PRRowView`，仿 issue 行（状态点取首个标签色），点击打开 PR 链接，
  无「详情」按钮。
- **会话行**：新增 `SessionRowView`，来源圆点（codex/claude 配色）+ 标题 + 项目名 +
  相对时间；点击 → `showMainWindow()` 并
  `webView.evaluateJavaScript("window.openSession('<id>')")`。

## 主窗口 `index.html`

- `SMODULES` 新增 `{id:"repos", name:"仓库"}`；`renderModule` 注册 `renderRepos`。
- 仓库列表从 `renderIssue` 迁出，独立为 `renderRepos`。
- `repoRow` 每行增加「关注」开关。
- `normalizeCfg`：给旧 repo 补 `watch:true`（`watch===undefined` 时）。
- 「Issue 过滤」模块只保留 issue 过滤项，加一行指引指向「仓库」模块。
- 暴露 `window.openSession(id)`：切到会话视图、清空搜索筛选、调用现有
  `openSession(id)`、滚动入视。

## 风险

- `gh pr list --search` 双次调用去重 —— 按 `Number` 去重，`author` 来源优先。
- 旧配置兼容 —— 自定义 `UnmarshalJSON` 解决。
- Swift 菜单内重建项 —— 段控件 action 里 `removeAllItems` 后重新填充。
