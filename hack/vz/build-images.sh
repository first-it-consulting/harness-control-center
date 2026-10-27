#!/usr/bin/env bash
# Builds the vz provider's guest kernel and images (ADR-0016) on a Mac,
# in a Linux container (Docker), until the release pipeline publishes
# them. Every input is pinned and checked against its publisher's
# checksum before use.
#
#   hack/vz/build-images.sh [OUT]
#
# OUT (default: ~/Library/Application Support/hcc/local/images) gets
# kernel/Image, base/rootfs.ext4 and claude-code/{rootfs.ext4,tools.ext4,
# tools.txt}. Parts whose inputs did not change are kept.
#
# HCC_TOOLS lists the Alpine packages the claude-code image offers as
# tools (ADR-0010); set it to "" for none.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
OUT=${1:-"$HOME/Library/Application Support/hcc/local/images"}
CACHE=${HCC_BUILD_CACHE:-"$HOME/Library/Caches/hcc-build"}

# shellcheck source=hack/vz/inputs.sh
. "$HERE/inputs.sh"
TOOLS=${HCC_TOOLS-$DEFAULT_TOOLS}

[[ $(uname -s) == Darwin && $(uname -m) == arm64 ]] || { echo "this builds arm64 images on an Apple Silicon Mac" >&2; exit 1; }
command -v docker >/dev/null || { echo "needs Docker (a Linux container builds the images)" >&2; exit 1; }
check_tools "$TOOLS"
mkdir -p "$CACHE" "$OUT"

fetch "$KERNEL_URL" "sha256:$KERNEL_SHA256" "$CACHE/linux-$KERNEL_VERSION.tar.xz"
fetch "$ALPINE_URL" "sha256:$ALPINE_SHA256" "$CACHE/alpine-minirootfs-$ALPINE_VERSION-aarch64.tar.gz"
fetch "$CC_URL" "$CC_INTEGRITY" "$CACHE/claude-code-linux-arm64-musl-$CC_VERSION.tgz"

echo "== hcc-guest (linux/arm64)"
(cd "$REPO" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$CACHE/hcc-guest-linux-arm64" ./cmd/hcc-guest)

echo "== building in $BUILDER"
docker run --rm --platform linux/arm64 \
	-v "$REPO/deploy/images:/src/images:ro" \
	-v "$HERE/build-in-container.sh:/src/build.sh:ro" \
	-v "$CACHE:/cache:ro" \
	-v "$OUT:/out" \
	-e KERNEL_VERSION="$KERNEL_VERSION" -e ALPINE_VERSION="$ALPINE_VERSION" -e CC_VERSION="$CC_VERSION" -e TOOLS="$TOOLS" \
	-e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
	"$BUILDER" sh /src/build.sh
echo "== images in $OUT"
