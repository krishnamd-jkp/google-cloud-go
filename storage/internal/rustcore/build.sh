#!/usr/bin/env bash
# Builds the Rust GCS engine (gcs_core v2) and installs the static library where the cgo
# bindings link it from: internal/rustcore/lib/<goos>_<goarch>/libgcs_core.a
#
# Usage (from anywhere):
#   storage/internal/rustcore/build.sh            # release build, ring crypto provider (default)
#   CRYPTO=aws-lc storage/internal/rustcore/build.sh
#
# Requirements: rustup/cargo (https://rustup.rs). The aws-lc provider additionally needs cmake
# and a C compiler; the default ring provider does not.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if ! command -v cargo >/dev/null 2>&1 && [ -f "$HOME/.cargo/env" ]; then
  # shellcheck disable=SC1091
  . "$HOME/.cargo/env"
fi
command -v cargo >/dev/null || { echo "cargo not found; install Rust via https://rustup.rs" >&2; exit 1; }

features=()
case "${CRYPTO:-ring}" in
  ring) ;;
  aws-lc) features=(--no-default-features --features crypto-aws-lc) ;;
  *) echo "CRYPTO must be ring or aws-lc" >&2; exit 1 ;;
esac

goos="$(go env GOOS)"
goarch="$(go env GOARCH)"
echo "rustc: $(rustc --version)"
echo "building gcs_core for ${goos}/${goarch} (crypto=${CRYPTO:-ring}) ..."
(cd "$here/gcs_core" && cargo build --release --locked "${features[@]}")

target_dir="${CARGO_TARGET_DIR:-$here/gcs_core/target}"
mkdir -p "$here/lib/${goos}_${goarch}"
cp "$target_dir/release/libgcs_core.a" "$here/lib/${goos}_${goarch}/libgcs_core.a"
echo "installed $here/lib/${goos}_${goarch}/libgcs_core.a ($(du -h "$here/lib/${goos}_${goarch}/libgcs_core.a" | cut -f1))"
