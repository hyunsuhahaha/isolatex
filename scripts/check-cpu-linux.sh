#!/usr/bin/env bash
# Use a private systemd delegation; never edit global controller policy.
set -euo pipefail
if [[ "${1:-}" == --delegated ]]; then
  if [[ $# != 2 || "$(id -u)" == 0 ]]; then exit 2; fi
  parent="${ISOLATEX_TEST_CGROUP_PARENT:-}"
  if [[ "$parent" != /sys/fs/cgroup/system.slice/isolatex-cpu-test-*.service ]]; then
    echo 'expected a private CPU test service delegation' >&2
    exit 2
  fi
  echo '+cpu +memory +pids' > "$parent/cgroup.subtree_control"
  "$2" test -race ./internal/supervisor -run 'TestWorkerCPUBudget|TestCPUBudget' -count=2 -timeout=90s
  exit
fi
if [[ $# != 2 || "$(id -u)" != 0 ]]; then
  echo 'usage: sudo bash scripts/check-cpu-linux.sh <test-user> <absolute-go-binary>' >&2
  exit 2
fi
test_user="$1"
go_binary="$2"
if [[ "$(id -u -- "$test_user")" == 0 || "$go_binary" != /* || ! -x "$go_binary" ]]; then
  echo 'a non-root test user and absolute executable Go path are required' >&2
  exit 2
fi
if [[ "$(stat -fc %T /sys/fs/cgroup)" != cgroup2fs ]]; then
  echo 'cgroup v2 is required' >&2
  exit 1
fi
script="$(realpath "$0")"
cd "$(dirname "$script")/.."
unit="isolatex-cpu-test-$(date +%s)-$$"
cleanup() {
  result=$?
  trap - EXIT
  # The service manager owns this exact private unit and its descendants.
  systemctl stop "$unit.service" >/dev/null 2>&1 || true
  exit "$result"
}
trap cleanup EXIT
systemd-run --quiet --wait --collect --pipe --service-type=exec --unit="$unit" \
  --property="User=$test_user" --property='Delegate=cpu memory pids' \
  --property=DelegateSubgroup=manager --property=OOMPolicy=continue \
  --property=KillMode=control-group --working-directory="$PWD" \
  --setenv="PATH=$(dirname "$go_binary"):$PATH" \
  --setenv="ISOLATEX_TEST_CGROUP_PARENT=/sys/fs/cgroup/system.slice/$unit.service" \
  /bin/bash "$script" --delegated "$go_binary"
