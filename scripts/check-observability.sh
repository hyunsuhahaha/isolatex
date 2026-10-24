#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
promtool="${1:-promtool}"
"$promtool" check config examples/observability/prometheus.yml
"$promtool" test rules examples/observability/alerts.test.yml
