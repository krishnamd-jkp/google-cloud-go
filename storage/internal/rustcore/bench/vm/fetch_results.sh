#!/usr/bin/env bash
# Copies all runs (~/results on the VM) to ./rustbench-results/<VM>/ (workstation).
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
source "$here/config.sh"
dest="${DEST:-$PWD/rustbench-results/$VM}"
mkdir -p "$dest"
gcloud compute scp --recurse "${GCLOUD_FLAGS[@]}" "$VM:~/results" "$dest/"
echo "copied to $dest/results:"
ls -1 "$dest/results"
