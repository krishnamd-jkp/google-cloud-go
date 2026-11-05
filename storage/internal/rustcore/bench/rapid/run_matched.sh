#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Matched-connection run: the two 48-worker Bidi workloads with the same number of gRPC connections
# on both engines (rapidbench --grpc-conns, default 192), like the Java PoC's matched run (48 each).
#
#   4  16 MiB reads of a 200 MB object, 48 workers   bidi, regional bucket
#   5  200 MB appendable writes, 48 workers          bidi, zonal bucket
#
# A 45 s smoke test of both engines runs first (SKIP_SMOKE=1 skips it): each run must complete
# operations with at most 1% errors. Then each workload runs on both engines back to back (5 min
# after a 30 s warm-up, 60 s cooldown), alternating which engine goes first. Then Cloud Monitoring
# CPU/memory, latencies and summary.md, as run_all.sh.
#
# Usage (on the VM, next to a rapidbench binary built with -tags gcsrust):
#   REGIONAL=<bucket> ZONAL=<zonal bucket> [CONNS=192] bash run_matched.sh
set -uo pipefail

: "${REGIONAL:?set REGIONAL to the regional bucket}"
: "${ZONAL:?set ZONAL to the zonal (Rapid) bucket}"
CONNS=${CONNS:-192}
COOLDOWN=${COOLDOWN:-60}
HERE=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-$HERE/rapidbench}
OUT=${OUT:-$HOME/rapid-results/$(date -u +%Y%m%dT%H%M%SZ)-matched-${CONNS}conn}
if [ ! -x "$BIN" ]; then
  echo "missing $BIN: go build -tags gcsrust -o rapidbench ./internal/rustcore/cmd/rapidbench" >&2
  exit 1
fi

mkdir -p "$OUT"
exec > >(tee -a "$OUT/run_all.log") 2>&1
echo "results: $OUT (gRPC connections on both engines: $CONNS)"

RUNS=(
  "--workload=read --transport=bidi --bucket=$REGIONAL --bucket-label=regional"
  "--workload=write --transport=bidi --bucket=$ZONAL --bucket-label=zonal"
)

describe() {
  local d="$*"
  d=${d//--bucket=$REGIONAL /}
  d=${d//--bucket=$ZONAL /}
  echo "$d"
}

run_one() { # engine, out, extra, args...
  local engine=$1 out=$2 extra=$3
  shift 3
  echo "== $(date -u +%T) $engine-${CONNS}conn $(describe "$@") $extra"
  # shellcheck disable=SC2086
  "$BIN" --engine="$engine" "$@" --grpc-conns="$CONNS" $extra --label="$engine-${CONNS}conn" --out="$out" \
    2>"$out/.stderr" \
    | grep -E '^(results:)|"(ops|errors|latency_ms_p50|latency_ms_p99|throughput_MBps|grpc_connections|tcp_established_443_mid)"' \
    | sed -e 's/^results: .*\//results: /'
  grep -E 'worker [0-9]+:|creating' "$out/.stderr" | head -5
}

echo "== smoke (45 s per run)"
if [ -n "${SKIP_SMOKE:-}" ]; then
  echo "skipped (SKIP_SMOKE)"
else
  mkdir -p "$OUT/smoke"
  for r in "${RUNS[@]}"; do
    for e in go rust; do
      # shellcheck disable=SC2086
      run_one "$e" "$OUT/smoke" "--duration=45 --warmup=5" $r
      sleep 5
    done
  done
  rm -f "$OUT/smoke/.stderr"
  # A run fails the smoke test if it completed nothing or more than 1% of its operations failed
  # (a rare transient server error is reported in the results, not treated as a setup problem).
  bad=$(python3 - "$OUT/smoke" <<'EOF'
import glob, json, sys
n = 0
for f in glob.glob(sys.argv[1] + "/*/summary.json"):
    s = json.load(open(f))
    if not s["ops"] or s["errors"] > s["ops"] / 100:
        print(f"{s['label']} {s['op']}: {s['errors']} errors, {s['ops']} ops", file=sys.stderr)
        n += 1
print(n)
EOF
  )
  if [ "$bad" != "0" ]; then
    echo "smoke test failed ($bad runs); stopping"
    exit 1
  fi
  sleep "$COOLDOWN"
fi

i=0
for r in "${RUNS[@]}"; do
  if ((i % 2 == 0)); then order=(go rust); else order=(rust go); fi
  for e in "${order[@]}"; do
    # shellcheck disable=SC2086
    run_one "$e" "$OUT" "" $r
    sleep "$COOLDOWN"
  done
  i=$((i + 1))
done
rm -f "$OUT/.stderr"

echo "== waiting 240 s for Cloud Monitoring"
sleep 240
python3 "$HERE/cloud_metrics.py" "$OUT"/*/summary.json >/dev/null || echo "cloud metrics failed (continuing)"
python3 "$HERE/monitoring_csv.py" "$OUT" >"$OUT/monitoring.csv" || echo "monitoring.csv failed (continuing)"
python3 "$HERE/latencies.py" "$OUT" "$OUT/latency" >/dev/null
python3 "$HERE/summarize.py" "$OUT" >"$OUT/summary.md"
echo "done: $OUT/summary.md"
