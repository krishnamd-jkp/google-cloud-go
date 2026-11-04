#!/usr/bin/env bash
# Copies the local storage module to the VM (workstation): the working tree as it is, including
# uncommitted changes and new files, but not git-ignored build outputs (the Rust core is rebuilt
# on the VM). Nothing is pushed to any git remote. Re-run after every local change.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
source "$here/config.sh"
repo="$(git -C "$here" rev-parse --show-toplevel)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Provenance, recorded in every result line (RUSTBENCH_SOURCE).
rev="$(git -C "$repo" rev-parse --short HEAD)"
branch="$(git -C "$repo" rev-parse --abbrev-ref HEAD)"
changed="$(git -C "$repo" status --porcelain -- storage | wc -l | tr -d ' ')"
echo "branch $branch at $rev with $changed uncommitted path(s) under storage/; packed $(date -u +%FT%TZ) on $(hostname)" > "$tmp/SOURCE"

# Tracked and untracked (not ignored) files that exist in the working tree.
git -C "$repo" ls-files -co --exclude-standard -- storage |
  while IFS= read -r f; do [[ -f "$repo/$f" ]] && printf '%s\n' "$f"; done > "$tmp/files"
tar -C "$repo" -czf "$tmp/src.tgz" -T "$tmp/files"
echo "packed $(wc -l < "$tmp/files" | tr -d ' ') files ($(du -h "$tmp/src.tgz" | cut -f1)): $(cat "$tmp/SOURCE")"

gcloud compute scp "${GCLOUD_FLAGS[@]}" "$tmp/src.tgz" "$tmp/SOURCE" "$here/setup_vm.sh" "$here/run_bench.sh" "$VM:~/"
gcloud compute ssh "$VM" "${GCLOUD_FLAGS[@]}" --command \
  'set -e; rm -rf ~/src; mkdir -p ~/src; tar -xzf ~/src.tgz -C ~/src; chmod +x ~/setup_vm.sh ~/run_bench.sh; echo "unpacked into ~/src"'
echo
echo "next (first time, or after changing Rust code): gcloud compute ssh $VM ${GCLOUD_FLAGS[*]} --command '~/setup_vm.sh'"
