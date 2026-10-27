#!/usr/bin/env bash
# Builds the vz release images (ADR-0016) on a Linux arm64 machine with
# Docker (the release pipeline's arm64 runner), from the same pinned
# inputs and container script as build-images.sh, but without Claude
# Code, which is not ours to redistribute.
#
#   hack/vz/release-images.sh OUT
#
# OUT gets the compressed release files, the kernel's source and
# configuration (the kernel is GPL-2.0; its source travels with it) and
# images.txt, one "file sha256" line per image file, which the release
# bakes into hcc so it verifies what it downloads.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
OUT=${1:?usage: release-images.sh OUT}
CACHE=${HCC_BUILD_CACHE:-$REPO/.cache/vz}

# shellcheck source=hack/vz/inputs.sh
. "$HERE/inputs.sh"
TOOLS=${HCC_TOOLS-$DEFAULT_TOOLS}

[[ $(uname -s) == Linux && $(uname -m) == aarch64 ]] || { echo "this runs on Linux arm64" >&2; exit 1; }
check_tools "$TOOLS"
# Parts whose inputs did not change are kept in $CACHE/out (the pipeline
# caches the kernel, the slow part).
BUILD=$CACHE/out
mkdir -p "$CACHE" "$OUT" "$BUILD"

fetch "$KERNEL_URL" "sha256:$KERNEL_SHA256" "$CACHE/linux-$KERNEL_VERSION.tar.xz"
fetch "$ALPINE_URL" "sha256:$ALPINE_SHA256" "$CACHE/alpine-minirootfs-$ALPINE_VERSION-aarch64.tar.gz"

echo "== hcc-guest (linux/arm64)"
(cd "$REPO" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$CACHE/hcc-guest-linux-arm64" ./cmd/hcc-guest)

echo "== building in $BUILDER"
docker run --rm --platform linux/arm64 \
	-v "$REPO/deploy/images:/src/images:ro" \
	-v "$HERE/build-in-container.sh:/src/build.sh:ro" \
	-v "$CACHE:/cache:ro" \
	-v "$BUILD:/out" \
	-e KERNEL_VERSION="$KERNEL_VERSION" -e ALPINE_VERSION="$ALPINE_VERSION" -e CC_VERSION= -e TOOLS="$TOOLS" \
	-e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
	"$BUILDER" sh /src/build.sh

echo "== release files in $OUT"
pack() { gzip -n -9 <"$BUILD/$1" >"$OUT/$2"; }
pack kernel/Image vz-kernel-arm64.gz
pack base/rootfs.ext4 vz-base-arm64.ext4.gz
pack claude-code/rootfs.ext4 vz-claude-code-arm64.ext4.gz
if [[ -f $BUILD/claude-code/tools.ext4 ]]; then
	pack claude-code/tools.ext4 vz-claude-code-tools-arm64.ext4.gz
	cp "$BUILD/claude-code/tools.txt" "$OUT/vz-claude-code-tools-arm64.txt"
fi
cp "$BUILD/kernel/config" "$OUT/vz-kernel-arm64.config"
cp "$CACHE/linux-$KERNEL_VERSION.tar.xz" "$OUT/linux-$KERNEL_VERSION.tar.xz"
(cd "$OUT" && for f in vz-*; do
	[[ $f == *.config ]] || echo "$f $(sha256_of "$f")"
done) >"$OUT/images.txt"
cat "$OUT/images.txt"
