# 用量报表与 Hash 版本发布设计

## 背景

现有用量报表已经从 ccusage 获取会话级输入 Token、缓存 Token、输出 Token和成本，
但趋势、来源占比、项目排行和模型排行主要按成本展示，Token 信息不完整。
现有发布流程使用 SemVer 标签，用户希望改用当前 Git 提交的短 hash 标识版本。

## 用量报表设计

报表继续使用现有会话聚合接口，不新增后端统计维度。总 Token 统一按
`输入 Token + 缓存 Token + 输出 Token` 计算。

- 周期概览同时显示成本、总 Token 和会话数。
- 用量趋势为每个时间桶同时呈现成本与总 Token；两种指标独立归一化，避免单位不同造成误导。
- 来源占比提供成本占比和 Token 占比两组比例条，并显示各来源的实际金额、总 Token和百分比。
- Top 项目和模型用量每行同时显示成本与总 Token，支持按总 Token或成本排序；切换排序不隐藏另一项指标。
- 图表和排行的悬停信息包含会话数、输入 Token、缓存 Token、输出 Token、总 Token和成本。
- 成本或 Token 总数为零时显示 0%，不产生除零或虚假满条。
- 保持现有朴素数字卡和微型柱图风格，不引入可视化依赖。

## Hash 版本发布设计

新版本统一使用 `git rev-parse --short=7 HEAD` 得到的 7 位短 hash，例如 `53a82fa`。

- App 内版本、Git Tag、GitHub Release 标题和构建产物文件名使用相同短 hash。
- 发布必须在工作区干净且当前提交已经推送到远端后进行，保证 Release 能准确追溯源码。
- 发布脚本构建 App 和 ZIP，创建短 hash Tag，推送 Tag，并通过 GitHub CLI 创建 Release。
- 历史 SemVer Release 保留，不重写已有标签。
- 自动更新不再比较 SemVer 大小，而是比较最新 Release 标签与当前构建版本：不同则提示更新，相同则视为最新。
- GitHub 的 latest Release 接口负责确定最新正式发布，草稿和预发布继续忽略。

## 数据流与错误处理

报表仍由 `/api/stats` 返回会话数组，前端按选中周期汇总并渲染双指标。
重新计算失败时保留现有错误提示和已有缓存。

发布脚本在 GitHub CLI 未登录、工作区不干净、分支未推送、Tag 或 Release 已存在、
构建失败或上传失败时立即退出，不吞掉 Git push 错误。

## 验证

- 使用确定性样例验证周期、来源、项目和模型的成本与 Token 聚合。
- 验证零成本、零 Token、空数据和单一来源的比例展示。
- 运行 Go 测试和格式检查。
- 按项目要求执行 `./build-mac.sh`，确认 Go 与 Swift 编译和 App 打包成功。
- 提交并推送后执行 hash 发布流程，核对 Tag、Release 和 ZIP 文件名一致。
