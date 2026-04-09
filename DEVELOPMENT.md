# FileFlow Bridge 开发环境

这是一个用于开发和调试 FileFlow Bridge 的容器化开发环境。

基础构建命令、运行命令和环境变量全集已收口到 [docs/agents/runtime-and-commands.md](docs/agents/runtime-and-commands.md)。本文件只保留开发容器和调试相关流程，避免与用户文档和代理文档重复。

## 快速开始

### 使用开发管理脚本

```bash
# 启动开发环境
./dev-manager.sh start

# 或使用菜单
./dev-manager.sh menu
```

### 手动启动

```bash
# 构建并启动开发环境
docker-compose -f docker-compose.dev.yaml up -d

# 查看日志
docker-compose -f docker-compose.dev.yaml logs -f
```

## 开发环境特性

- 实时代码变更检测（热重载）
- 详细的调试日志 (DEBUG 级别)
- 源代码卷挂载，便于修改
- 完整的 Go 开发工具链
- 应用在 `/app/bridge` 目录下运行

## 端口映射

- HTTP: `8000` -> `8000` (可在 `.env` 中自定义)
- TCP: `8888` -> `8888` (可在 `.env` 中自定义)

## 文件挂载

- `/bridge` 目录挂载到容器的 `/app/bridge`
- 日志持久化到 `fileflow_dev_logs` 卷
- 数据持久化到 `fileflow_dev_data` 卷

## 开发工作流

1. 启动开发环境
2. 在本地编辑 `bridge/` 目录下的 Go 代码
3. 应用会自动重新编译和运行
4. 检查日志和功能

## 管理命令

```bash
# 启动开发环境
./dev-manager.sh start

# 停止开发环境
./dev-manager.sh stop

# 重启开发环境
./dev-manager.sh restart

# 查看实时日志
./dev-manager.sh logs

# 进入容器进行调试
./dev-manager.sh shell

# 重建开发镜像
./dev-manager.sh build
```

## 配置来源

开发环境仍然通过 `.env` 文件控制端口和日志级别，但变量说明不再在这里重复展开；请直接查看 [docs/agents/runtime-and-commands.md](docs/agents/runtime-and-commands.md)。

## 异常测试入口

仓库内现已提供可直接执行的异常测试入口：

```bash
# Go 基础验证
npm test
npm run vet

# 仅浏览器异常场景
npm run browser:anomaly

# 统一运行 go test / go vet / 临时 Bridge / 浏览器异常场景
npm run anomaly:all

# 发布前检查
npm run release:check
```

详细说明见：

- [docs/browser-anomaly-tests.md](docs/browser-anomaly-tests.md)
- [docs/all-anomaly-tests.md](docs/all-anomaly-tests.md)

## 生产环境部署

对于生产环境，请使用标准的 `docker-compose.yaml`：

```bash
# 启动生产环境
docker-compose up -d
```
