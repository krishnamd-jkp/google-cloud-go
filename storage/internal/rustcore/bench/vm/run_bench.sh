#!/usr/bin/env bash
# Runs ON the VM: the benchmark suite, then the Markdown report. Takes about an hour with the
# default profile and 5 rounds; start it with start_remote.sh so it survives ssh disconnects.
#
#   BUCKET=my-bucket [PROFILE=default|quick] [ROUNDS=5] [BACKENDS=go-json,rust-json] \
#     [CASES=read-16MiB-c8,list-10k-c1] [CLEANUP=1] ~/run_bench.sh
#
# Results: ~/results/<run id>/{results.jsonl,report.md,suite.log,matrix.txt}.
set -euo pipefail
: "${BUCKET:?set BUCKET}"
PROFILE="${PROFILE:-default}"
ROUNDS="${ROUNDS:-5}"
PREFIX="${PREFIX:-rustbench/$(hostname)}"
RUN_ID="${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}"
OUT="$HOME/results/$RUN_ID"
mkdir -p "$OUT"

# CloudPath for every client: the Rust SDK has no DirectPath (rustbench enforces this as well).
export GOOGLE_CLOUD_DISABLE_DIRECT_PATH=true
RUSTBENCH_SOURCE="$(cat ~/SOURCE 2>/dev/null || echo unknown)"
export RUSTBENCH_SOURCE

args=(suite -bucket "$BUCKET" -prefix "$PREFIX" -profile "$PROFILE" -rounds "$ROUNDS" -run-id "$RUN_ID" -out "$OUT/results.jsonl")
if [[ -n "${BACKENDS:-}" ]]; then args+=(-backends "$BACKENDS"); fi
if [[ -n "${CASES:-}" ]]; then args+=(-cases "$CASES"); fi

{
  echo "run $RUN_ID on $(hostname): profile $PROFILE, $ROUNDS rounds, gs://$BUCKET/$PREFIX"
  echo "source: $RUSTBENCH_SOURCE"
  echo
  ~/bin/rustbench matrix -profile "$PROFILE"
} > "$OUT/matrix.txt"

~/bin/rustbench "${args[@]}" 2>&1 | tee "$OUT/suite.log"
~/bin/rustbench-report -run "$RUN_ID" "$OUT/results.jsonl" > "$OUT/report.md"
echo "report: $OUT/report.md"

if [[ "${CLEANUP:-0}" == "1" ]]; then
  # Deletes the datasets (and leftovers of interrupted runs) under the prefix only.
  ~/bin/rustbench cleanup -bucket "$BUCKET" -prefix "$PREFIX"
fi
