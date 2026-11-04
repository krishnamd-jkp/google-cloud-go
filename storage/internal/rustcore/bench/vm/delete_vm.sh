#!/usr/bin/env bash
# Deletes the benchmark VM (workstation). Fetch the results first (fetch_results.sh). Asks for
# confirmation. Bucket data is not touched: delete it with `rustbench cleanup` (or CLEANUP=1).
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
source "$here/config.sh"
read -r -p "Delete VM $VM in $ZONE (project $PROJECT)? Type the VM name to confirm: " answer
if [[ "$answer" != "$VM" ]]; then
  echo "not deleted"
  exit 1
fi
gcloud compute instances delete "$VM" "${GCLOUD_FLAGS[@]}" --quiet
