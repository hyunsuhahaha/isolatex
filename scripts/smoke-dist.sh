#!/usr/bin/env bash
set -euo pipefail
if [[ $# -lt 1 || $# -gt 3 ]]; then
  echo 'usage: smoke-dist.sh <unpacked-native-directory> [gateway-origin] [admin-origin]' >&2
  exit 2
fi
package_dir="$(cd -- "$1" && pwd -P)"
repo_root="$(cd "$(dirname "$0")/.." && pwd -P)"
gateway_url="${2:-http://127.0.0.1:8080}"
admin_url="${3:-http://127.0.0.1:9090}"
if [[ ! -x "$package_dir/bin/isolatex" || ! -f "$package_dir/isolatex.yaml" ]]; then
  echo 'expected an unpacked native Linux/macOS distribution' >&2
  exit 1
fi
mkdir -p "$repo_root/.tools"
scratch="$(mktemp -d "$repo_root/.tools/smoke-dist.XXXXXX")"
runtime_pid=''
completed=false
cleanup() {
  result=$?
  trap - EXIT
  set +e
  if [[ -n "$runtime_pid" ]]; then
    kill -TERM "$runtime_pid" 2>/dev/null
    for ((attempt=0; attempt<100; attempt++)); do
      if ! kill -0 "$runtime_pid" 2>/dev/null; then break; fi
      sleep 0.1
    done
    if kill -0 "$runtime_pid" 2>/dev/null; then
      echo 'packaged runtime exceeded shutdown wait' >&2
      kill -KILL "$runtime_pid" 2>/dev/null
      result=1
    fi
    if ! wait "$runtime_pid"; then
      echo 'packaged runtime exited unsuccessfully' >&2
      result=1
    fi
  fi
  if [[ "$completed" != true && "$result" -eq 0 ]]; then result=1; fi
  if [[ "$result" -ne 0 && -f "$scratch/runtime.log" ]]; then
    cat "$scratch/runtime.log" >&2
  fi
  # Only remove the two files this helper creates; never recursively remove a
  # caller-provided distribution or another directory's contents.
  if ! rm -f -- "$scratch/bench" "$scratch/runtime.log"; then result=1; fi
  if ! rmdir -- "$scratch"; then result=1; fi
  if [[ "$result" -eq 0 ]]; then echo 'native_distribution_smoke_passed'; fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
(cd "$repo_root" && go build -o "$scratch/bench" ./bench)
# Only the HTTP load driver is built from source. Both runtime and demo workers
# execute from the extracted package, including its relative command paths.
(cd "$package_dir" && exec ./bin/isolatex -config isolatex.yaml) >"$scratch/runtime.log" 2>&1 &
runtime_pid=$!
ready=false
for ((attempt=0; attempt<100; attempt++)); do
  if ! kill -0 "$runtime_pid" 2>/dev/null; then exit 1; fi
  if curl -fsS -- "$admin_url/readyz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.1
done
if [[ "$ready" != true ]]; then exit 1; fi
"$scratch/bench" -url "$gateway_url" -admin "$admin_url" -expected-runtime-pid "$runtime_pid"
"$scratch/bench" -url "$gateway_url" -admin "$admin_url" -expected-runtime-pid "$runtime_pid" \
  -healthy-group beta -healthy-path /b/work -fault-group alpha -fault-path /a/work
completed=true
