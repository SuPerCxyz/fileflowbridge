# FileFlow Bridge 测试报告（2026-04-08 / 2026-04-09）

本报告只保留当前仍有效的测试结论，并把已修复问题收敛为历史摘要，避免旧失败记录继续污染当前判断。

## 当前结论

当前仓库已经具备可重复执行的发布前验证入口：

```bash
npm run release:check
```

该入口会自动完成：

1. 启动临时 Bridge 容器
2. 执行 `go test ./...`
3. 执行 `go vet ./...`
4. 执行正式浏览器异常场景
5. 清理临时 Bridge 容器和网络

当前已验证为通过。

## 当前有效结果

### Go 级验证

- `go test ./...`：通过
- `go vet ./...`：通过

### 正式浏览器异常场景

| 场景 | 当前结果 | 说明 |
| --- | --- | --- |
| `abandon_cleanup` | 通过 | 上传页关闭后 token 清理为 `404` |
| `same_token_race` | 通过 | 一位下载者 `200`，另一位受控失败 `409` |
| `slow_consumer_large` | 通过 | 大文件慢消费下载完整，哈希一致 |
| `mixed_concurrent_browser_uploads` | 通过 | 小/中/大三种文件并发上传下载均哈希一致 |
| `bridge_restart_before_download` | 通过 | Bridge 重启后旧链接返回 `404` |

### 真实链路验证结论

- `CLI_TCP` 小文件延迟下载：通过
- `CLI_TCP` 大文件慢下载：通过
- `CLI_TCP` 发送端中断：最终清理通过，当前实测清理窗口约 `3.3s`
- `HTTP_MULTIPART` 小文件正常链路：通过
- `HTTP_MULTIPART` 大文件慢下载：通过
- `WEB_WS` 正常上传下载：通过
- `WEB_WS` 上传后直接关闭页面：通过

## 当前状态

截至本报告更新时，之前已发现的核心阻塞问题均已修复，并已通过：

- `go test ./...`
- `go vet ./...`
- `npm run release:check`

当前未保留新的阻塞级缺陷。

后续若继续优化，可关注：

- `CLI_TCP` 发送端中断后的清理窗口是否还要从约 `3.3s` 继续压缩
- 是否要把更多 CLI / multipart 场景也正式并入统一异常脚本

## 正式入口

### npm 入口

```bash
npm test
npm run vet
npm run browser:anomaly
npm run anomaly:all
npm run release:check
```

### 关键文档

- `docs/project-user-manual.md`
- `docs/abnormal-test-matrix-2026-04-08.md`
- `docs/browser-anomaly-tests.md`
- `docs/all-anomaly-tests.md`

## 历史摘要（已修复）

以下问题曾被实测发现，但当前已不再作为有效阻塞结论保留：

- CLI 小文件延迟下载后 token 提前清理
- CLI 大文件慢下载内容截断
- Web abandon 上传残留 token
- 同 token 双下载触发 websocket 并发写 panic
- 下载端/发送端中断被误记为“传输完成”

这些问题的详细排查过程已体现在 Git 历史与相关测试代码中，不再在当前报告中逐条保留原始失败现场。
