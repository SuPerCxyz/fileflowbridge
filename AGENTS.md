# AGENTS.md - FileFlow Bridge 代理入口

本文件只保留代理执行任务时最需要的入口信息。详细说明拆分到 `docs/agents/`，避免把构建命令、编码规范、验证要求混在一个长文件里。

## 项目定位

- FileFlow Bridge 是一个 Go 编写的文件流桥接工具，核心目标是零等待分发和低内存占用。
- 涉及桥接传输链路的改动时，优先关注内存效率、网络可靠性和错误处理。
- 安全令牌必须继续使用加密安全随机数生成。

## 代理工作方式

- 先阅读 [README.md](README.md) 和任务相关代码，再决定修改范围。
- 默认做最小充分修改，避免顺手扩张到无关模块。
- 保留中文注释风格；新增注释只写必要信息。
- 不要运行破坏性 Git 命令；未经明确要求，不修改历史、不清理他人改动。

## 首选验证

- 格式化：`go fmt ./...`
- 静态检查：`go vet ./...`
- 构建桥接端：`go build -o /tmp/fileflowbridge bridge/main.go`
- 构建提供端：`go build -o /tmp/fileflowprovider provider/main.go`

按改动范围选择最小充分验证。若无法执行关键验证，必须明确说明原因。

## 文档索引

- [docs/agents/runtime-and-commands.md](docs/agents/runtime-and-commands.md)：构建、运行、开发命令与环境变量
- [docs/agents/coding-standards.md](docs/agents/coding-standards.md)：Go 编码约定、并发、日志与 HTTP 处理约束
- [docs/agents/testing-and-ops.md](docs/agents/testing-and-ops.md)：验证方式、项目结构与代理注意事项
- [README.md](README.md)：用户视角的使用与部署说明
- [DEVELOPMENT.md](DEVELOPMENT.md)：开发环境与容器化调试说明

修改前先看入口，遇到细节再跳转到对应文档，不再把全部规则堆在一个文件里。
