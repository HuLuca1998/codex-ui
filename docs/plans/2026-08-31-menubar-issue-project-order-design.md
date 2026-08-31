# 菜单栏 issue 按 GitHub Project 看板顺序排序

## 背景

菜单栏 issue 标签页把所有关注仓库的 issue 混在一起，按 `updatedAt` 倒序平铺。
更新时间反映的是「谁最近被碰过」，跟实际要干的先后没关系：
刚回了一句评论的收尾任务会顶到最前，真正排在队首待处理的需求反而被挤到列表尾巴外。

用户日常的优先级排布在 GitHub Project 看板里（如 `BDBGAME2024` 的 `pp-game 任务追踪` #11），
看板里条目的先后就是干活顺序。菜单栏应当照搬这份顺序，而不是另立一套。

## 目标

- 菜单栏 issue 顺序 = Project 看板里的条目顺序，看板怎么拖菜单栏就怎么显示。
- 看板绑定可配置（设置 → 仓库 → 看板），不硬编码任何 project 编号。
- 未绑定看板时行为不变，仍是更新时间倒序。
- 看板拉取失败不能拖垮 issue 列表本身。

## 数据来源

`gh project item-list <number> --owner <owner> --format json --limit 500`
返回的 `items` 顺序即看板顺序，每项 `content` 带 `number` 与 `repository`（`owner/name`）。
一个看板可跨仓库，所以顺序表的键取 `owner/repo#number`，不能只用 issue 编号。

草稿条目（`content.number == 0`）没有对应 issue，跳过。

## 实现

配置：`RepoMap` 新增 `project int`（`0` = 不排序）。owner 从 `repo` 的 `owner/name` 前半段取。

`refreshIssues()` 里，每个关注仓库拉完 issue 后，若配了看板就调 `ghProjectOrder()`
汇总成一张 `issueKey → 位置` 表；拉失败只往 `issuesErr` 追加一条，issue 列表照常返回。

`sortIssues(all, order, repoIdx)` 三级比较：

1. 在看板里的排在看板外的前面；
2. 都在看板里 → 先按仓库在配置里的次序分组，组内按看板位置升序；
3. 都不在看板里 → 按 `updatedAt` 倒序。

`order` 为空时三条规则退化成纯更新时间倒序，即未配置看板时的老行为。

菜单栏 Swift 侧不用改：`/api/menubar` 的 `issues` 数组顺序就是渲染顺序。

## 设置页

「仓库」模块每行新增「看板」下拉：`不排序 — 按更新时间倒序` + 该 owner 名下的开放看板。
选项来自新接口 `/api/gh/projects?owner=`（`gh project list`，过滤掉已关闭看板），
按 owner 缓存，多行仓库共用；仓库名改动后重新拉取。

## 代价

`refreshIssues` 每轮（默认 5 分钟）多一次 `gh project item-list` 调用，实测约 4 秒 / 113 条。
只在配了看板的仓库上发生，且与 issue 拉取串行在同一次刷新里，菜单栏读的是缓存，不受影响。
