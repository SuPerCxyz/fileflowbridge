#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://host.docker.internal:8000}"
DATA_DIR="${DATA_DIR:-/tmp/ffb-testdata}"
SCENARIOS="${SCENARIOS:-abandon_cleanup,same_token_race,slow_consumer_large}"
IMAGE="${IMAGE:-mcr.microsoft.com/playwright:v1.59.1-noble}"
RESTART_COMMAND="${RESTART_COMMAND:-}"

run_node() {
  local scenarios="$1"
  local output_path="${2:-}"
  docker run --rm \
    --add-host host.docker.internal:host-gateway \
    -v "$(pwd):/work" \
    -v "${DATA_DIR}:/data" \
    -w /work \
    "${IMAGE}" \
    bash -lc "npm init -y >/dev/null 2>&1 && npm install playwright@1.59.1 >/dev/null 2>&1 && BASE_URL='${BASE_URL}' DATA_DIR='/data' SCENARIOS='${scenarios}' OUTPUT_PATH='${output_path}' node scripts/browser_anomaly_tests.js"
}

json_escape_file() {
  python3 - <<'PY' "$1"
import json, sys, pathlib
print(json.dumps(json.loads(pathlib.Path(sys.argv[1]).read_text()), ensure_ascii=False))
PY
}

if [[ "${SCENARIOS}" == *"bridge_restart_before_download"* ]]; then
  if [[ -z "${RESTART_COMMAND}" ]]; then
    echo "RESTART_COMMAND is required when SCENARIOS contains bridge_restart_before_download" >&2
    exit 1
  fi

  tmp_prepare="$(mktemp "${DATA_DIR%/}/browser-prepare-XXXXXX.json")"
  run_node "prepare_restart_before_download" "/data/$(basename "${tmp_prepare}")" >/dev/null
  prepared_json="$(json_escape_file "${tmp_prepare}")"

  download_url="$(python3 - <<'PY' "${tmp_prepare}"
import json, sys, pathlib
data = json.loads(pathlib.Path(sys.argv[1]).read_text())
print(data[0]["downloadUrl"])
PY
)"
  download_url="${download_url/host.docker.internal/127.0.0.1}"
  expected_sha="$(python3 - <<'PY' "${tmp_prepare}"
import json, sys, pathlib
data = json.loads(pathlib.Path(sys.argv[1]).read_text())
print(data[0]["expectedSha256"])
PY
)"

  eval "${RESTART_COMMAND}"
  sleep 5

  tmp_download="$(mktemp "${DATA_DIR%/}/browser-restart-download-XXXXXX.bin")"
  http_status="$(curl -sS -o "${tmp_download}" -w "%{http_code}" "${download_url}" || true)"
  actual_sha=""
  if [[ -s "${tmp_download}" ]]; then
    actual_sha="$(sha256sum "${tmp_download}" | awk '{print $1}')"
  fi

  restart_result="$(python3 - <<'PY' "${prepared_json}" "${http_status}" "${expected_sha}" "${actual_sha}"
import json, sys
prepared = json.loads(sys.argv[1])[0]
print(json.dumps([{
    "scenario": "bridge_restart_before_download",
    "token": prepared["token"],
    "status": int(sys.argv[2]) if sys.argv[2].isdigit() else sys.argv[2],
    "expectedSha256": sys.argv[3],
    "actualSha256": sys.argv[4] or None
}], ensure_ascii=False))
PY
)"

  other_scenarios="${SCENARIOS//bridge_restart_before_download/}"
  other_scenarios="${other_scenarios//,,/,}"
  other_scenarios="${other_scenarios#,}"
  other_scenarios="${other_scenarios%,}"

  if [[ -n "${other_scenarios}" ]]; then
    other_json="$(run_node "${other_scenarios}")"
    python3 - <<'PY' "${restart_result}" "${other_json}"
import json, sys
left = json.loads(sys.argv[1])
right = json.loads(sys.argv[2])
print(json.dumps(left + right, ensure_ascii=False, indent=2))
PY
  else
    python3 - <<'PY' "${restart_result}"
import json, sys
print(json.dumps(json.loads(sys.argv[1]), ensure_ascii=False, indent=2))
PY
  fi
  exit 0
fi

run_node "${SCENARIOS}"
