# Integer configuration settings

Request capacities, body/connection limits, worker counts, ports, readiness thresholds,
circuit-breaker minimum request counts, resource limits and the configuration version
must use YAML integer scalars. IsolateX rejects fractional values, scientific-notation
floats, quoted numbers, booleans, explicit null and out-of-range integers at startup.
Omit optional settings to retain their defaults; use an integer `0` only where the
setting documents zero as valid. Existing positivity, range and capacity-sum checks
still apply.

For example, `queue_size: -0.9` previously became zero, `max_concurrent: 2.9` became
two, and `failure_threshold: 0.9` became zero and selected the default threshold.
These inputs now fail `isolatex -config config.yaml -check` rather than silently
changing admission or health policy. Fix the YAML to express the intended whole
number before deploying. Configuration errors occur before listeners or workers
start.

YAML integer aliases and mapping merges remain supported. Integer values supplied
through aliases or merged defaults receive the same check, including values in a
merged mapping that an explicit key overrides. Unknown fields and duplicate keys
remain errors. String duration settings such as `request_timeout: 2s` and fractional
ratios such as `failure_ratio: 0.5` are unchanged. Programmatically constructed Go
configuration values continue to use the existing Go field types and `Validate`.

The configuration input remains capped at 1 MiB. Integer validation traverses the
schema and YAML nodes with a maximum recursion depth of 100; excessively nested
or cyclic input fails instead of recursing without a bound.
