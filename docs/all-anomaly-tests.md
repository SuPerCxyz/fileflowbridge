# 统一异常测试入口

仓库现在提供统一入口脚本：

- `scripts/run_all_anomaly_tests.sh`

作用：

1. 自动选择空闲端口
2. 启动临时 Bridge 容器
3. 运行 `go test ./...`
4. 运行 `go vet ./...`
5. 运行正式浏览器异常测试脚本
6. 自动清理临时 Bridge 容器和网络

## 默认覆盖场景

- `abandon_cleanup`
- `same_token_race`
- `slow_consumer_large`
- `mixed_concurrent_browser_uploads`
- `bridge_restart_before_download`

## 运行方式

```bash
./scripts/run_all_anomaly_tests.sh
```

已验证：

- 该脚本可以真实完成“起临时 Bridge -> 跑 `go test` / `go vet` -> 跑全部正式浏览器异常场景 -> 清理环境”的完整闭环。

指定测试文件目录：

```bash
DATA_DIR=/tmp/ffb-testdata ./scripts/run_all_anomaly_tests.sh
```

只跑部分浏览器场景：

```bash
SCENARIOS=abandon_cleanup,same_token_race ./scripts/run_all_anomaly_tests.sh
```

如果你希望把它当作发布前标准门禁，也可以直接使用：

```bash
npm run release:check
```

## 说明

- 该脚本是“总控入口”，浏览器异常场景细节仍由 `scripts/run_browser_anomaly_tests.sh` 执行。
- `bridge_restart_before_download` 场景需要宿主机可执行 `docker restart`，总控脚本会自动把临时 Bridge 容器名传给浏览器脚本。
