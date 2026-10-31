# HTTP failure diagnostics and bounded logging

The CLI uses one asynchronous log writer with at most 256 queued records and
64 KiB per record. A stalled destination, full queue or oversized record does
not wait on request/worker control paths; lost records increase
`isolatex_dropped_log_records_total`. Delivery is best effort. Shutdown allows
at most 250ms for this writer to drain.

ReverseProxy diagnostics use the Gateway instance's supplied logger, with
`level: ERROR` and the owning `group`. Native public/admin HTTP server diagnostics
use the supplied runtime logger, with `level: ERROR` and `listener: public` or
`listener: admin`. These are fixed configuration identities, not request-derived
metric labels. The standard-library diagnostic text remains the message; server
panic messages can include a stack and connection address. Request-summary debug
logs keep their existing fixed outcomes.

All of these paths share the existing bounded writer and dropped-record counter;
there is no additional unbounded diagnostic queue. A truncated upstream response
still counts as a stream failure and releases its request slot. HTTP server panic
recovery can close the failed connection even while log delivery is stalled.
Dropping a diagnostic does not convert a failed HTTP response into success.

The CLI already calls slog.SetDefault, which bridges the process-global standard
logger into its handler. Explicit per-instance ErrorLog bridges remove reliance
on that ambient setting, supply attribution, and set error severity rather than
inheriting the standard logger's default level. The usual CLI setup was already
using its bounded writer. A custom caller supplying a synchronous blocking slog
handler remains responsible for that handler's behavior; the bridge cannot make
an arbitrary handler nonblocking.

Serial regression tests deliberately block an unrelated global standard logger
and the configured bounded destination. Real truncated HTTP responses and public/
admin panic connections must finish, protected traffic must keep responding, and
queue drops must be counted. Separate actual-request checks drain the writer and
verify JSON severity and group/listener fields. Tests restore the global logger
and release blocked writers before closing their test servers.
