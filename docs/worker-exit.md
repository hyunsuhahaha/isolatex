# Unconfirmed worker exit

A successful native termination request does not prove that a process has exited.
A failed OS termination or a process stuck in a kernel wait can prevent `Cmd.Wait`
from finishing. Waiting indefinitely for one worker would keep every concurrent
`Supervisor.Stop` caller waiting as well.

For each worker, IsolateX requests normal termination and allows one second for
`Wait` to finish. It then requests forced termination and waits at most three
additional seconds for exit confirmation. This includes headroom for the existing
one-second `Cmd.WaitDelay` pipe drain. The waiting budget is independent per slot;
workers stop concurrently. These numbers bound the channel wait, not arbitrary
native system calls, process creation or total runtime shutdown time.

If exit is still unconfirmed, the supervisor attempts the generation's native
cleanup and releases the slot from monitoring as `failed`, unready and with no
replacement. A later successful cleanup does not erase the earlier uncertainty.
The original background `Wait` can still reap the process if it eventually exits;
its buffered result channel cannot strand that goroutine on delivery. This does
not guarantee that an unkillable process has disappeared or released resources.

The same policy applies to a readiness startup timeout. Both the startup timeout
and unconfirmed-exit cause are preserved. Healthy groups continue to run while
that slot is being stopped; they do not lend it capacity or restart with it.

Inspect `/status` and `worker_resource_cleanup_failed` logs. If any slot had an
unconfirmed teardown in this lifetime, final shutdown logs
`worker_shutdown_incomplete` instead of `workers_stopped`, including when startup
cleanup failed earlier. Check the OS process tree, Job/cgroup and service manager
before restarting the runtime. A service manager's final kill policy is still
needed, especially for a runtime that itself crashes or is forcibly killed.

Tests first reproduce an exit-confirmation channel that never completes. Native
integration tests launch real HTTP workers and fault-inject ineffective termination
requests until cleanup, for both startup timeout and runtime shutdown. They check
bounded return, terminal slot state, no replacement, and an unaffected group's
HTTP/PID during startup teardown. They model failed termination requests; they do
not claim to induce a real uninterruptible kernel wait.
