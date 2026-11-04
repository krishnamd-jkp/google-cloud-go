#!/usr/bin/env bash
# Creates the benchmark VM (workstation). Only creates it: nothing is installed or run until
# push_code.sh and setup_vm.sh.
#
# The VM reaches the bucket with its service account. The Compute Engine default service account
# usually has access to buckets of its own project; otherwise set SERVICE_ACCOUNT and grant it:
#
#   gcloud storage buckets add-iam-policy-binding gs://$BUCKET \
#     --member="serviceAccount:$SERVICE_ACCOUNT" --role=roles/storage.objectAdmin
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
source "$here/config.sh"

args=(
  "${GCLOUD_FLAGS[@]}"
  --machine-type="$MACHINE_TYPE"
  --image-family=debian-12 --image-project=debian-cloud
  --boot-disk-size=50GB --boot-disk-type=pd-balanced
  # gVNIC: the higher-bandwidth virtual NIC (the default virtio-net caps throughput earlier).
  --network-interface=nic-type=GVNIC,network-tier=PREMIUM
  --scopes=cloud-platform
  --labels=purpose=rustbench
)
if [[ -n "$SERVICE_ACCOUNT" ]]; then
  args+=(--service-account="$SERVICE_ACCOUNT")
fi

echo "creating VM $VM: $MACHINE_TYPE in $ZONE (project $PROJECT)"
gcloud compute instances create "$VM" "${args[@]}"
echo
echo "next: $here/push_code.sh"
