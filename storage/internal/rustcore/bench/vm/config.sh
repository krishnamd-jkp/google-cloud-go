# Settings shared by the scripts that run on your workstation (create_vm.sh, push_code.sh,
# start_remote.sh, fetch_results.sh, delete_vm.sh). PROJECT and BUCKET are required; override
# any of the others in the environment:
#
#   PROJECT=my-project BUCKET=my-bucket MACHINE_TYPE=c3-standard-22 ZONE=us-east4-a ./create_vm.sh

PROJECT="${PROJECT:?set PROJECT to the Google Cloud project for the VM}"
# Keep the VM in the bucket's location for real runs (cross-continent latency would dominate
# small-object cases).
ZONE="${ZONE:-us-central1-a}"
VM="${VM:-rustbench-${USER:-bench}}"
# 16 vCPUs, up to 32 Gbps egress. Use the same machine type for every run you compare.
MACHINE_TYPE="${MACHINE_TYPE:-n2-standard-16}"
BUCKET="${BUCKET:?set BUCKET to the bucket to benchmark against}"
# Empty: the project's Compute Engine default service account. It needs read/write access to
# the bucket (see create_vm.sh).
SERVICE_ACCOUNT="${SERVICE_ACCOUNT:-}"

GCLOUD_FLAGS=(--project="$PROJECT" --zone="$ZONE")
