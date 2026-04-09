#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA_DIR="${DATA_DIR:-/tmp/ffb-testdata}"
IMAGE="${IMAGE:-golang:1.24}"
BROWSER_IMAGE="${BROWSER_IMAGE:-mcr.microsoft.com/playwright:v1.59.1-noble}"
SCENARIOS="${SCENARIOS:-abandon_cleanup,same_token_race,slow_consumer_large,mixed_concurrent_browser_uploads,bridge_restart_before_download}"

read -r HTTP_PORT TCP_PORT < <(
  python3 - <<'PY'
import socket
for start in range(65000, 64000, -2):
    sockets = []
    ok = True
    for port in (start, start + 1):
        sock = socket.socket()
        try:
            sock.bind(("127.0.0.1", port))
            sockets.append(sock)
        except OSError:
            ok = False
            break
    for sock in sockets:
        sock.close()
    if ok:
        print(start, start + 1)
        break
else:
    raise SystemExit("no free port pair found")
PY
)

RUN_ID="ffb-anomaly-$$"
NETWORK_NAME="${RUN_ID}-net"
BRIDGE_NAME="${RUN_ID}-bridge"

cleanup() {
  docker rm -f "${BRIDGE_NAME}" >/dev/null 2>&1 || true
  docker network rm "${NETWORK_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "${DATA_DIR}"

echo "[1/4] 创建隔离网络 ${NETWORK_NAME}"
docker network create "${NETWORK_NAME}" >/dev/null

echo "[2/4] 启动临时 Bridge 容器 ${BRIDGE_NAME} (${HTTP_PORT}/${TCP_PORT})"
docker run -d \
  --name "${BRIDGE_NAME}" \
  --network "${NETWORK_NAME}" \
  -p "${HTTP_PORT}:${HTTP_PORT}" \
  -p "${TCP_PORT}:${TCP_PORT}" \
  -v "${ROOT_DIR}:/src" \
  -w /src/bridge \
  "${IMAGE}" \
  sh -lc "export PATH=/usr/local/go/bin:/go/bin:\$PATH; go run main.go --http-port=${HTTP_PORT} --tcp-port=${TCP_PORT} --max-file-size=1 --token-len=8" >/dev/null

for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:${HTTP_PORT}/health" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo "[3/4] 运行 Go 验证"
(
  cd "${ROOT_DIR}"
  go test ./...
  go vet ./...
)

echo "[4/4] 运行正式浏览器异常场景"
(
  cd "${ROOT_DIR}"
  BASE_URL="http://host.docker.internal:${HTTP_PORT}" \
  DATA_DIR="${DATA_DIR}" \
  IMAGE="${BROWSER_IMAGE}" \
  SCENARIOS="${SCENARIOS}" \
  RESTART_COMMAND="docker restart ${BRIDGE_NAME} >/dev/null" \
  ./scripts/run_browser_anomaly_tests.sh
)
