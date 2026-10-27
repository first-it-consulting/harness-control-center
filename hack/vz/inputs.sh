# shellcheck shell=bash disable=SC2034 # the variables are for the scripts that source this
# The vz images' pinned inputs (ADR-0016), shared by build-images.sh (on a
# Mac) and release-images.sh (in the release pipeline). Every input is
# checked against its publisher's checksum before use.

BUILDER="alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a"
KERNEL_VERSION=6.18.59
KERNEL_SHA256=53a55ac9863a5547e292b8e381d4e4c2240dd1127d5428a7c44dbc550be23963 # kernel.org sha256sums.asc
ALPINE_VERSION=3.24.3
ALPINE_SHA256=4d3f80afaebd447c5591a63a73bde380a149fe8b2d85488cafabb2ff97551f50
# Claude Code is not ours to redistribute: release images leave it out, and
# hcc fetches it on the user's machine (internal/local keeps its own pin).
CC_VERSION=2.1.310
CC_INTEGRITY="sha512-w0h8eJdDUmnCAtMSthCBC8AeB0nmnBhueVD9XPyPOxC8Sf+4zLy6k0hbAC+StonX80M0f7YVzHaQ9VXtNSxlWQ=="
# The tool catalog's Alpine packages (ADR-0010).
DEFAULT_TOOLS="python3 py3-pip nodejs npm make gcc musl-dev curl go"

KERNEL_URL="https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KERNEL_VERSION.tar.xz"
ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION%.*}/releases/aarch64/alpine-minirootfs-$ALPINE_VERSION-aarch64.tar.gz"
CC_URL="https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64-musl/-/claude-code-linux-arm64-musl-$CC_VERSION.tgz"

sha256_of() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# fetch URL CHECK DEST: CHECK is "sha256:HEX" or an npm "sha512-BASE64".
fetch() {
	local url=$1 check=$2 dest=$3
	ok() {
		case $check in
		sha256:*) [[ $(sha256_of "$1") == "${check#sha256:}" ]] ;;
		sha512-*) [[ "sha512-$(openssl dgst -sha512 -binary "$1" | base64 | tr -d '\n')" == "$check" ]] ;;
		*) return 1 ;;
		esac
	}
	if [[ -f $dest ]] && ok "$dest"; then return 0; fi
	echo "== fetching $url"
	curl -fsSL --proto '=https' --tlsv1.2 -o "$dest.part" "$url"
	ok "$dest.part" || { echo "CHECKSUM MISMATCH for $url" >&2; rm -f "$dest.part"; exit 1; }
	mv "$dest.part" "$dest"
}

check_tools() {
	local p
	for p in $1; do
		[[ $p =~ ^[a-z0-9][a-z0-9._+-]{0,127}$ ]] || { echo "invalid tool package name: $p" >&2; exit 1; }
	done
}
