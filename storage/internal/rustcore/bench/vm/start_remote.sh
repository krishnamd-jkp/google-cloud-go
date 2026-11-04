#!/usr/bin/env bash
# Starts run_bench.sh on the VM in the background (workstation) and returns immediately; the run
# survives ssh disconnects. Same variables as run_bench.sh:
#
#   [PROFILE=quick] [ROUNDS=1] [BACKENDS=...] [CASES=...] [CLEANUP=1] ./start_remote.sh
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
source "$here/config.sh"

vars="BUCKET=$BUCKET PROFILE=${PROFILE:-default} ROUNDS=${ROUNDS:-5} CLEANUP=${CLEANUP:-0}"
if [[ -n "${BACKENDS:-}" ]]; then vars+=" BACKENDS=$BACKENDS"; fi
if [[ -n "${CASES:-}" ]]; then vars+=" CASES=$CASES"; fi

gcloud compute ssh "$VM" "${GCLOUD_FLAGS[@]}" --command \
  "nohup env $vars ~/run_bench.sh > ~/rustbench-run.log 2>&1 < /dev/null & echo \"started ($vars)\""
echo "follow:  gcloud compute ssh $VM ${GCLOUD_FLAGS[*]} --command 'tail -f ~/rustbench-run.log'"
echo "results: $here/fetch_results.sh"
