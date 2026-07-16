# GitHub Actions 云端自动发布设计

## 目标

推送到 `main` 后，由 GitHub Actions 在 ARM64 macOS Runner 上自动测试、构建并发布
Codex Viewer。GitHub CLI 用于手动触发、查看运行状态和创建 Release。

## 触发与权限

- `push` 到 `main` 自动触发。
- `workflow_dispatch` 支持通过 `gh workflow run` 手动重试。
- Workflow 显式声明 `contents: write`，用于创建 Tag、Release 和上传资产。
- 使用发布并发组串行化 `main` 的发布任务，避免多个任务乱序覆盖 Latest。

## Runner 与版本

使用公开仓库免费的标准 `macos-15` Runner。GitHub 官方将其定义为 ARM64 M1，
因此 `build-mac.sh` 生成的产物名为 `Codex-Viewer-<hash>-arm64.zip`。

版本统一取当前工作流提交的 7 位短 hash：

```bash
VERSION="${GITHUB_SHA::7}"
```

该值同时写入 Go 二进制、App Info.plist、ZIP 文件名、Git Tag、Release 标题和
Release Tag，保证 App 显示、自动更新与下载资产一致。

## 工作流

1. Checkout 触发工作流的提交。
2. 运行 `go test ./...`、`go vet ./...`、Shell 语法检查和前端 JavaScript 语法检查。
3. 执行 `VERSION=<hash> ./build-mac.sh --zip`。
4. 校验 Runner 架构、Info.plist 版本和 ZIP 文件名。
5. 使用 GitHub CLI 检查同 hash Release 是否已经存在；存在则安全结束。
6. 使用 `gh release create` 指向完整 `GITHUB_SHA` 创建同名 Tag 和 Latest Release。
7. 上传 ARM64 ZIP，并使用 GitHub 自动生成的 Release notes。

## 并发与失败处理

- 发布任务使用固定并发组，避免同一分支多个发布同时写 Release。
- 构建或校验失败时不创建 Tag 和 Release。
- Release 已存在时返回成功，支持手动重跑工作流。
- `gh release create` 失败时保留完整 Actions 日志，不吞掉退出码。
- Workflow 只响应 `main`，功能分支推送不会创建正式版本。

## CLI 使用

推送后自动触发：

```bash
git push origin main
```

手动触发和监控：

```bash
gh workflow run cloud-release.yml --ref main
gh run watch
gh run list --workflow cloud-release.yml
```

## App 更新兼容

App 已使用 hash 作为当前版本，并按 Latest Release 标签是否不同判断新版本。
旧 SemVer 构建会将新的 hash Release 识别为可用更新；安装后当前 hash 与 Latest
一致时显示为最新版本。

## 验证

- 本地解析 workflow YAML 并检查 Shell 片段语法。
- 推送后使用 `gh run watch` 等待首次 Actions 运行完成。
- 核对 Runner 架构为 ARM64、Release Tag 为推送 commit 的 7 位 hash。
- 核对 Release 资产文件名、Info.plist 版本和 GitHub Latest Release 一致。
