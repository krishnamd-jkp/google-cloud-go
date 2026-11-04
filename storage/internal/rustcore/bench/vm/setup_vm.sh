#!/usr/bin/env bash
# Runs ON the VM: installs Go and Rust, builds the Rust core on the VM itself (so the static
# library matches the VM's toolchain and glibc) and the benchmark binaries. Safe to re-run; it
# rebuilds from ~/src (refresh it with push_code.sh).
set -euo pipefail
GO_VERSION="${GO_VERSION:-1.26.8}"         # must satisfy the go directive of storage/go.mod
RUST_TOOLCHAIN="${RUST_TOOLCHAIN:-stable}" # the core needs Rust >= 1.85 (edition 2024)

sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq build-essential ca-certificates curl git

if [[ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null || true)" != "go${GO_VERSION}" ]]; then
  echo "installing Go ${GO_VERSION}"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf /tmp/go.tgz
fi
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"

if [[ ! -x "$HOME/.cargo/bin/cargo" ]]; then
  echo "installing Rust (${RUST_TOOLCHAIN})"
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain "$RUST_TOOLCHAIN"
fi
# shellcheck source=/dev/null
source "$HOME/.cargo/env"

cd ~/src/storage
./internal/rustcore/build.sh # release build: about 3 minutes on 16 vCPUs
mkdir -p ~/bin
go build -tags gcsrust -o ~/bin/rustbench ./internal/rustcore/cmd/rustbench
go build -o ~/bin/rustbench-report ./internal/rustcore/cmd/rustbench-report

echo
go version
rustc --version
~/bin/rustbench matrix
echo
echo "setup done. Next, from your workstation: start_remote.sh (or on the VM: BUCKET=... ~/run_bench.sh)"
