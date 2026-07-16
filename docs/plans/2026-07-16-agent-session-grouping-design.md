# Claude 与 Codex Agent 会话聚合设计

## 背景

Claude teammate 会话可能只有独立 JSONL，team-lead 的 JSONL 已被清理，导致同一团队的几十条会话平铺。Claude 文件头的 `aiTitle` 还可能完全相同，无法区分各 teammate。

Codex subagent 则在首条 `session_meta` 中保存完整关联：当前 thread `id`、根 thread `session_id`、直接父级 `parent_thread_id`、任务路径 `agent_path` 和昵称 `agent_nickname`。本机数据存在多层 subagent，但根会话文件目前完整。

## 目标

- Claude teammate 按 `teamName` 聚合；父会话缺失时显示虚拟团队行。
- Codex subagent 按根 `session_id` 聚合；保留直接父级和任务路径元数据。
- 两种来源共享一致的折叠、搜索、排序、计数和活动状态行为。
- 子项标题优先表达 agent 身份或任务，不再重复公共会话标题。
- 虚拟分组只承担导航，不伪装成可打开、可续聊或可置顶的真实会话。

## 数据模型

`Summary` 增加团队与直接父级字段：

- `teamName`：Claude 团队标识。
- `teamTitle`：Claude 公共 `aiTitle`，供团队行使用。
- `directParentSid`：Codex 的 `parent_thread_id`。

Claude 仅在事件同时带有非空 `teamName` 时，把顶层 `agentName` 视为真实 teammate 名；文件头 `type=agent-name` 的重复 AI 标题不能覆盖它。

Codex 子会话只消费第一条 `session_meta`。`session_id != id` 时视为 subagent，按根 `session_id` 聚合；`parent_thread_id` 仅保留树关系，不直接制造多层侧栏。

## 标题规则

Claude teammate：

1. 真实 `agentName`。
2. 首条 teammate 任务文本。
3. 公共 `aiTitle`。

Claude 虚拟团队行优先使用公共 `aiTitle`；没有时显示 `Claude 团队 · <teamName>`。

Codex subagent：

1. `agent_path` 最后一段任务名。
2. `agent_nickname`。
3. 原有消息标题回退。

完整 `agent_path` 保留在预览/搜索信息中，昵称作为辅助元数据展示。

## 前端聚合

先处理显式 `parentSid`，再处理 Claude `teamName`：

- 有真实父项时使用真实父项。
- Claude team-lead 不存在时生成仅存在于渲染层的虚拟父项。
- Codex 根会话缺失时也生成同样的虚拟父项作为防御性兜底。
- 多层 Codex subagent 默认压平到根会话下一层，避免大型任务形成难以操作的深树。
- 搜索命中任一子项时保留并展开整个组。
- 组的排序和活跃状态聚合所有子项。
- 会话总数只统计真实会话；虚拟父项不增加计数。

点击真实父项打开会话；点击虚拟父项只展开或折叠。虚拟父项不允许置顶、续聊或加载详情。

## 验证

- Go 单元测试覆盖 Claude 团队字段解析、标题优先级，以及 Codex 根/直接父级和任务标题解析。
- 使用本机真实 Claude/Codex 数据检查 API 输出中的关联字段。
- 构造前端会话数据验证：缺失父会话时生成一行分组、子项标题唯一、总数不包含虚拟行。
- 运行 `go test ./...`。
- 按项目要求运行 `./build-mac.sh`，确认 Go、嵌入前端和 Swift App 均成功打包。
