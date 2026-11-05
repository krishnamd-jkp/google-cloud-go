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

# Runs every workload once on both engines (the Go SDK's defaults vs the Rust core's defaults), one
# run at a time with a cooldown between runs, then fetches Cloud Monitoring CPU/memory and writes
# summary.md. Mirrors the Java PoC's run_all.sh (java-storage/rust-poc-benchmark/scripts):
#
#   1  Open (100 MiB and 100 KiB)   bidi
#   2  Read50x2x4 concurrent/serial bidi, json
#   3  Random8K                     bidi, json
#   4  16 MiB reads, 48 workers     bidi, json
#   5  200 MB writes, 48 workers    bidi (appendable, zonal bucket), json (regional bucket)
#   6  List Objects / List Buckets  gRPC
#
# The same binary and settings for every run; only --engine switches the engine. CloudPath on both.
#
# Usage (on the VM, next to a rapidbench binary built with -tags gcsrust):
#   REGIONAL=<bucket> ZONAL=<zonal bucket> PROJECT=<project> bash run_all.sh   # full run (~2 h 25 min)
#   SMOKE=1 REGIONAL=... ZONAL=... PROJECT=... bash run_all.sh                  # 45 s per run (~30 min)
# Then: python3 charts.py <out> <out>/latency <out>/monitoring.csv <out>/charts
set -uo pipefail

: "${REGIONAL:?set REGIONAL to the regional bucket}"
: "${ZONAL:?set ZONAL to the zonal (Rapid) bucket}"
: "${PROJECT:?set PROJECT to the project whose buckets list-buckets lists}"
COOLDOWN=${COOLDOWN:-60}
SMOKE=${SMOKE:-}
HERE=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-$HERE/rapidbench}
OUT=${OUT:-$HOME/rapid-results/$(date -u +%Y%m%dT%H%M%SZ)${SMOKE:+-smoke}}
EXTRA=""
if [ -n "$SMOKE" ]; then
  EXTRA="--duration=45 --warmup=5"
  COOLDOWN=5
fi
if [ ! -x "$BIN" ]; then
  echo "missing $BIN: go build -tags gcsrust -o rapidbench ./internal/rustcore/cmd/rapidbench" >&2
  exit 1
fi

mkdir -p "$OUT"
exec > >(tee -a "$OUT/run_all.log") 2>&1
echo "results: $OUT"

# One line per workload variant: rapidbench arguments. Each runs on both engines.
RUNS=(
  "--workload=open --transport=bidi --bucket=$REGIONAL --bucket-label=regional"
  "--workload=open --transport=bidi --open-size=102400 --bucket=$REGIONAL --bucket-label=regional"
  "--workload=read50x2x4 --transport=bidi --mode=concurrent --bucket=$REGIONAL --bucket-label=regional"
  "--workload=read50x2x4 --transport=bidi --mode=serial --bucket=$REGIONAL --bucket-label=regional"
  "--workload=read50x2x4 --transport=json --mode=concurrent --bucket=$REGIONAL --bucket-label=regional"
  "--workload=read50x2x4 --transport=json --mode=serial --bucket=$REGIONAL --bucket-label=regional"
  "--workload=random8k --transport=bidi --bucket=$REGIONAL --bucket-label=regional"
  "--workload=random8k --transport=json --bucket=$REGIONAL --bucket-label=regional"
  "--workload=read --transport=bidi --bucket=$REGIONAL --bucket-label=regional"
  "--workload=read --transport=json --bucket=$REGIONAL --bucket-label=regional"
  "--workload=write --transport=bidi --bucket=$ZONAL --bucket-label=zonal"
  "--workload=write --transport=json --bucket=$REGIONAL --bucket-label=regional"
  "--workload=list-objects --transport=bidi --bucket=$REGIONAL --bucket-label=regional"
  "--workload=list-buckets --transport=bidi --project=$PROJECT"
)

# What the log shows for a run: the arguments without bucket and project names.
describe() {
  local d="$*"
  d=${d//--bucket=$REGIONAL /}
  d=${d//--bucket=$ZONAL /}
  d=${d// --project=$PROJECT/}
  echo "$d"
}

run_one() { # label, args...
  local label=$1 engine=${1%-default}
  shift
  echo "== $(date -u +%T) $label $(describe "$@")"
  # shellcheck disable=SC2086
  "$BIN" --engine="$engine" "$@" $EXTRA --label="$label" --out="$OUT" 2>"$OUT/.stderr" \
    | grep -E '^(results:)|"(ops|errors|latency_ms_p50|latency_ms_p99|throughput_MBps|items_per_sec|tcp_established_443_mid)"' \
    | sed -e 's/^results: .*\//results: /'
  grep -E 'worker [0-9]+:|creating' "$OUT/.stderr" | head -5
  sleep "$COOLDOWN"
}

# Create the test objects once, untimed: 200 MB (workload 4), 200 MiB (2), 100 MiB (1, 3), 100 KiB
# (1), and the 100 list objects (6).
echo "== prepare"
for a in "--workload=read" "--workload=read50x2x4" "--workload=random8k" "--workload=open --open-size=102400" \
  "--workload=list-objects"; do
  # shellcheck disable=SC2086
  "$BIN" --engine=go $a --bucket="$REGIONAL" --threads=1 --duration=1 --warmup=0 \
    --out="$OUT/prepare" >/dev/null 2>&1 || echo "prepare failed: $a"
done
rm -rf "$OUT/prepare"

i=0
for r in "${RUNS[@]}"; do
  # Alternate which engine goes first, so that neither always runs first.
  # shellcheck disable=SC2086
  if ((i % 2 == 0)); then
    run_one go-default $r
    run_one rust-default $r
  else
    run_one rust-default $r
    run_one go-default $r
  fi
  i=$((i + 1))
done
rm -f "$OUT/.stderr"

if [ -z "$SMOKE" ]; then
  # Cloud Monitoring ingests with a few minutes' delay.
  echo "== waiting 240 s for Cloud Monitoring"
  sleep 240
  python3 "$HERE/cloud_metrics.py" "$OUT"/*/summary.json >/dev/null || echo "cloud metrics failed (continuing)"
  python3 "$HERE/monitoring_csv.py" "$OUT" >"$OUT/monitoring.csv" || echo "monitoring.csv failed (continuing)"
fi
python3 "$HERE/latencies.py" "$OUT" "$OUT/latency" >/dev/null
python3 "$HERE/summarize.py" "$OUT" >"$OUT/summary.md"
echo "done: $OUT/summary.md"
