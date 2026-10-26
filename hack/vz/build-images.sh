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
TOOLS=${HCC_TOOLS-python3 py3-pip nodejs npm make gcc musl-dev curl go}

# Pinned inputs.
BUILDER="alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a"
KERNEL_VERSION=6.18.59
KERNEL_SHA256=53a55ac9863a5547e292b8e381d4e4c2240dd1127d5428a7c44dbc550be23963 # kernel.org sha256sums.asc
ALPINE_VERSION=3.24.3
ALPINE_SHA256=4d3f80afaebd447c5591a63a73bde380a149fe8b2d85488cafabb2ff97551f50
CC_VERSION=2.1.310
CC_INTEGRITY="sha512-w0h8eJdDUmnCAtMSthCBC8AeB0nmnBhueVD9XPyPOxC8Sf+4zLy6k0hbAC+StonX80M0f7YVzHaQ9VXtNSxlWQ=="

[[ $(uname -s) == Darwin && $(uname -m) == arm64 ]] || { echo "this builds arm64 images on an Apple Silicon Mac" >&2; exit 1; }
command -v docker >/dev/null || { echo "needs Docker (a Linux container builds the images)" >&2; exit 1; }
for p in $TOOLS; do
	[[ $p =~ ^[a-z0-9][a-z0-9._+-]{0,127}$ ]] || { echo "invalid tool package name: $p" >&2; exit 1; }
done
mkdir -p "$CACHE" "$OUT"

# fetch URL CHECK DEST: CHECK is "sha256:HEX" or an npm "sha512-BASE64".
fetch() {
	local url=$1 check=$2 dest=$3
	ok() {
		case $check in
		sha256:*) [[ $(shasum -a 256 "$1" | cut -d' ' -f1) == "${check#sha256:}" ]] ;;
		sha512-*) [[ "sha512-$(openssl dgst -sha512 -binary "$1" | base64)" == "$check" ]] ;;
		esac
	}
	if [[ -f $dest ]] && ok "$dest"; then return 0; fi
	echo "== fetching $url"
	curl -fsSL --proto '=https' --tlsv1.2 -o "$dest.part" "$url"
	ok "$dest.part" || { echo "CHECKSUM MISMATCH for $url" >&2; rm -f "$dest.part"; exit 1; }
	mv "$dest.part" "$dest"
}
fetch "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KERNEL_VERSION.tar.xz" "sha256:$KERNEL_SHA256" "$CACHE/linux-$KERNEL_VERSION.tar.xz"
fetch "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION%.*}/releases/aarch64/alpine-minirootfs-$ALPINE_VERSION-aarch64.tar.gz" "sha256:$ALPINE_SHA256" "$CACHE/alpine-minirootfs-$ALPINE_VERSION-aarch64.tar.gz"
fetch "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64-musl/-/claude-code-linux-arm64-musl-$CC_VERSION.tgz" "$CC_INTEGRITY" "$CACHE/claude-code-linux-arm64-musl-$CC_VERSION.tgz"

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
