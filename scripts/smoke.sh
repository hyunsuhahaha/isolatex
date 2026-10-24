#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p bin
config_path="${1:-isolatex.yaml}"
gateway_url="${2:-http://127.0.0.1:8080}"
admin_url="${3:-http://127.0.0.1:9090}"
go build -o bin/demo-app ./examples/demo-app
go build -o bin/isolatex ./cmd/isolatex
bin/isolatex -config "$config_path" -check
bin/isolatex -config "$config_path" >bin/smoke.log 2>&1 &
runtime_pid=$!
cleanup() {
  kill -TERM "$runtime_pid" 2>/dev/null || true
  wait "$runtime_pid" || true
}
trap cleanup EXIT
ready=false
for attempt in $(seq 1 100); do
  if ! kill -0 "$runtime_pid" 2>/dev/null; then
    cat bin/smoke.log
    exit 1
  fi
  if curl -fsS "$admin_url/readyz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.1
done
if [ "$ready" != true ]; then
  cat bin/smoke.log
  exit 1
fi
go run ./bench -url "$gateway_url" -admin "$admin_url" -expected-runtime-pid "$runtime_pid"
go run ./bench -url "$gateway_url" -admin "$admin_url" \
  -expected-runtime-pid "$runtime_pid" \
  -healthy-group beta -healthy-path /b/work \
  -fault-group alpha -fault-path /a/work
