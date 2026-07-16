# Codex 缓存 Token 统计修复设计

## 背景

Codex Viewer 使用 `ccusage codex session --json --offline` 读取本机会话用量。
当前解析器读取旧字段 `cachedInputTokens`，而 ccusage 20.0.17 返回
`cacheReadTokens` 和 `cacheCreationTokens`。因此 Codex 的缓存 Token 全部被记为 0，
本机总 Token 从实际约 3.89B 缩小为约 155M。

## 修复范围

本次只修复本地 ccusage 统计，不接入 OpenAI 服务端 Usage API。请求数、其他设备用量、
内部辅助模型和图像模型仍不属于本地会话统计口径。

## 兼容解析

Codex 会话解析同时声明以下字段：

- `cacheReadTokens`
- `cacheCreationTokens`
- `cachedInputTokens`
- `totalTokens`

缓存 Token 按以下顺序确定：

1. 优先使用 `cacheReadTokens + cacheCreationTokens`。
2. 新字段总和为 0 时，兼容读取 `cachedInputTokens`。
3. 显式缓存字段均为 0，但 `totalTokens` 大于输入与输出之和时，使用差值恢复缓存。
4. 差值不得为负数，真实零缓存会话保持为 0。

输入、缓存和输出相加后应与 ccusage 的 `totalTokens` 一致。`reasoningOutputTokens`
是输出 Token 的子集，不重复计入总量。

## 缓存失效与界面说明

- `statsVersion` 从 3 升到 4，使旧的错误统计缓存自动失效。
- 前端发现缓存失效且无数据时沿用现有逻辑自动重新计算。
- 报表说明明确数据来自本机 ccusage、会话数不等于 OpenAI 请求数、总 Token 包含缓存。

## 测试

为 Codex JSON 解析器增加表驱动回归测试，覆盖：

- ccusage 20.0.17 的新缓存字段。
- 旧版 `cachedInputTokens` 字段。
- 仅有 `totalTokens` 时的差值恢复。
- 真实零缓存。
- 新旧字段同时存在时优先使用新字段，避免重复计算。

验证还包括 `go test ./...`、`go vet ./...`、JavaScript 与 Shell 语法检查、
`./build-mac.sh` 本地打包，以及推送后 GitHub Actions ARM64 云构建和 Release 核对。
