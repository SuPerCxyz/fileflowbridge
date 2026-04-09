# 浏览器异常测试脚本

本目录外的浏览器异常测试现在统一由仓库脚本驱动，不再依赖临时 `hold_ms` 片段脚本。

## 文件

- `scripts/browser_anomaly_tests.js`
  作用：Playwright 场景执行器
- `scripts/run_browser_anomaly_tests.sh`
  作用：Docker 包装器，自动拉起 Playwright 容器并执行场景

## 当前场景

- `abandon_cleanup`
  作用：上传页拿到下载链接后直接关闭，验证 token 是否清理为 `404`
- `same_token_race`
  作用：同一 token 两个下载者并发竞争，验证不会 panic，且失败侧为受控失败
- `slow_consumer_large`
  作用：浏览器上传大文件，下载端按慢消费节奏读取，验证内容完整性
- `bridge_restart_before_download`
  作用：浏览器上传拿到下载链接后，先重启 Bridge，再验证旧链接行为
- `mixed_concurrent_browser_uploads`
  作用：三个浏览器上传并发跑小/中/大文件下载，验证混合并发一致性

## 运行方式

示例：

```bash
BASE_URL=http://127.0.0.1:8000 \
SCENARIOS=abandon_cleanup,same_token_race,slow_consumer_large \
./scripts/run_browser_anomaly_tests.sh
```

Bridge 重启场景示例：

```bash
BASE_URL=http://127.0.0.1:8000 \
SCENARIOS=bridge_restart_before_download \
RESTART_COMMAND='docker restart your-bridge-container' \
./scripts/run_browser_anomaly_tests.sh
```

混合并发浏览器上传示例：

```bash
BASE_URL=http://127.0.0.1:8000 \
SCENARIOS=mixed_concurrent_browser_uploads \
./scripts/run_browser_anomaly_tests.sh
```

默认值：

- `BASE_URL=http://host.docker.internal:8000`
- `DATA_DIR=/tmp/ffb-testdata`
- `SCENARIOS=abandon_cleanup,same_token_race,slow_consumer_large`
- `RESTART_COMMAND=` 仅在 `bridge_restart_before_download` 场景需要

## 输出

脚本输出 JSON 数组，每个元素对应一个场景结果。后续可以直接把结果接进测试报告或 CI 解析。
