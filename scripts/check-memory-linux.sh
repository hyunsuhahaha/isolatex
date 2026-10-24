#!/usr/bin/env bash
# Isolated privileged test setup; workers and Go tests run as the supplied user.
set -euo pipefail
if [[ $# != 2 || "$(id -u)" != 0 ]]; then
  echo 'usage: sudo bash scripts/check-memory-linux.sh <test-user> <absolute-go-binary>' >&2
  exit 2
fi
test_user="$1"
go_binary="$2"
test_uid="$(id -u "$test_user")"
test_gid="$(id -g "$test_user")"
if [[ "$test_uid" == 0 || "$go_binary" != /* || ! -x "$go_binary" ]]; then
  echo 'a non-root test user and absolute executable Go path are required' >&2
  exit 2
fi
if [[ "$(stat -fc %T /sys/fs/cgroup)" != cgroup2fs ]]; then
  echo 'cgroup v2 is required' >&2
  exit 1
fi
cd "$(dirname "$0")/.."
task_group="$(mktemp -d /sys/fs/cgroup/isolatex-test.XXXXXX)"
cleanup() {
  result=$?
  trap - EXIT
  # Remove only our known, empty directories. Never recursively delete cgroup
  # interfaces or change global controller policy to make a test pass.
  if [[ -d "$task_group/manager" ]]; then rmdir -- "$task_group/manager" || result=1; fi
  rmdir -- "$task_group" || result=1
  exit "$result"
}
trap cleanup EXIT
echo +memory > "$task_group/cgroup.subtree_control"
chown "$test_uid:$test_gid" "$task_group" "$task_group/cgroup.procs" "$task_group/cgroup.threads" "$task_group/cgroup.subtree_control"
mkdir "$task_group/manager"
(
  # The test owner must reside inside the delegation too: clone3 checks
  # migration permission on the common ancestor of owner and target cgroups.
  echo "$BASHPID" > "$task_group/manager/cgroup.procs"
  exec runuser -u "$test_user" -- env PATH="$(dirname "$go_binary"):$PATH" ISOLATEX_TEST_CGROUP_PARENT="$task_group" \
    "$go_binary" test -race ./internal/supervisor -run 'TestWorkerMemoryBudget|TestMemoryBudgetRejects' -count=3 -timeout=90s
)
