#!/bin/sh
# Runs inside the arm64 Alpine container started by build-images.sh, as
# root. Inputs (checked by the caller) are in /cache, sources in /src,
# results go to /out. No mounts and no privileges are needed: packages go
# into the image trees with "apk --root".
#
# With CC_VERSION empty, the claude-code image is built without Claude
# Code itself (release images: hcc adds it on the user's machine).
set -eu

apk add --no-cache -q bash e2fsprogs xz tar >/dev/null

stamp() { cat "$@" | sha256sum | cut -d' ' -f1; }
uptodate() { [ -f "/out/$1/.inputs" ] && [ "$(cat "/out/$1/.inputs")" = "$2" ]; }
finish() { echo "$2" >"/out/$1/.inputs"; chown -R "$HOST_UID:$HOST_GID" "/out/$1"; }

MINIROOT=/cache/alpine-minirootfs-$ALPINE_VERSION-aarch64.tar.gz

echo "== kernel $KERNEL_VERSION"
k_inputs=$(echo "$KERNEL_VERSION" | stamp - /src/images/kernel/vz-arm64.config)
if uptodate kernel "$k_inputs"; then
	echo "   up to date"
else
	apk add --no-cache -q build-base bc bison flex elfutils-dev openssl-dev perl linux-headers findutils diffutils >/dev/null
	cd /tmp
	tar -xJf "/cache/linux-$KERNEL_VERSION.tar.xz"
	cd "linux-$KERNEL_VERSION"
	make -s ARCH=arm64 defconfig
	scripts/kconfig/merge_config.sh -m -O . .config /src/images/kernel/vz-arm64.config >/dev/null
	make -s ARCH=arm64 olddefconfig
	# Every option of the fragment must have taken.
	bad=0
	while IFS= read -r line; do
		case "$line" in
		CONFIG_*=*) grep -qxF "$line" .config || { echo "kernel option did not take: $line"; bad=1; } ;;
		"# CONFIG_"*" is not set") opt=${line#\# }; opt=${opt%% *}; grep -q "^$opt=" .config && { echo "kernel option still set: $opt"; bad=1; } ;;
		esac
	done </src/images/kernel/vz-arm64.config
	[ "$bad" -eq 0 ] || exit 1
	make -s ARCH=arm64 -j"$(nproc)" Image
	mkdir -p /out/kernel
	cp arch/arm64/boot/Image /out/kernel/Image
	cp .config /out/kernel/config
	finish kernel "$k_inputs"
	cd / && rm -rf "/tmp/linux-$KERNEL_VERSION"
fi

# tree DIR: a fresh Alpine tree with the shared init library.
tree() {
	rm -rf "$1" && mkdir -p "$1"
	tar -xzf "$MINIROOT" -C "$1"
	install -D -m 0644 /src/images/common.sh "$1/usr/lib/hcc/common.sh"
	install -D -m 0755 /cache/hcc-guest-linux-arm64 "$1/usr/local/bin/hcc-guest"
}

# ext4 TREE OUT SIZE LABEL
ext4() { mkfs.ext4 -q -F -d "$1" -E root_owner=0:0 -L "$4" "$2" "$3"; }

echo "== image 'base'"
b_inputs=$(stamp /src/images/common.sh /src/images/base/init "$MINIROOT" /cache/hcc-guest-linux-arm64)
if uptodate base "$b_inputs"; then
	echo "   up to date"
else
	tree /tmp/base
	install -m 0755 /src/images/base/init /tmp/base/init
	mkdir -p /out/base
	ext4 /tmp/base /out/base/rootfs.ext4 64M hcc-base
	finish base "$b_inputs"
fi

echo "== image 'claude-code' (Claude Code ${CC_VERSION:-left out})"
CC_TGZ=/dev/null
[ -z "$CC_VERSION" ] || CC_TGZ=/cache/claude-code-linux-arm64-musl-$CC_VERSION.tgz
c_inputs=$(echo "$CC_VERSION tools: $TOOLS" | stamp - /src/images/common.sh /src/images/claude-code/init /src/images/claude-code/hcc-subagent "$MINIROOT" "$CC_TGZ" /cache/hcc-guest-linux-arm64 /src/build.sh)
if uptodate claude-code "$c_inputs"; then
	echo "   up to date"
else
	t=/tmp/cc
	tree $t
	install -m 0755 /src/images/claude-code/init $t/init
	install -m 0755 /src/images/claude-code/hcc-subagent $t/usr/local/bin/hcc-subagent
	if [ -n "$CC_VERSION" ]; then
		tar -xzf "$CC_TGZ" -C /tmp package/claude
		install -m 0755 /tmp/package/claude $t/usr/local/bin/claude
	fi
	# Signed Alpine packages, verified against the tree's own keys.
	apk_root() { apk --root $t --keys-dir $t/etc/apk/keys --repositories-file $t/etc/apk/repositories --arch aarch64 "$@"; }
	apk_root add --no-cache -q bash git jq ripgrep libgcc libstdc++ ca-certificates >/dev/null
	chroot $t /usr/sbin/adduser -D -u 1000 -s /bin/bash -h /home/agent agent
	[ -z "$CC_VERSION" ] || chroot $t /usr/local/bin/claude --version

	mkdir -p /out/claude-code
	rm -f /out/claude-code/tools.ext4 /out/claude-code/tools.txt
	if [ -n "$TOOLS" ]; then
		# The tool catalog (ADR-0010), as install-runner.sh builds it: each
		# package's signed file and those of the dependencies the image
		# lacks, then an offline test install from the files alone.
		cat=/tmp/catalog
		mkdir -p $cat/files
		installed=$(apk_root info -v 2>/dev/null)
		echo "# name version file... (built by build-images.sh)" >$cat/tools.txt
		for p in $TOOLS; do
			d=/tmp/fetch-$p
			mkdir $d
			apk_root fetch --no-cache --recursive --quiet -o $d "$p"
			own=$(cd $d && ls | grep -E "^$(echo "$p" | sed 's/+/\\+/g')-[0-9][^-]*-r[0-9]+\.apk\$" || true)
			[ "$(echo "$own" | wc -w)" -eq 1 ] || { echo "tool $p: cannot tell its package file"; exit 1; }
			version=${own#"$p"-}
			version=${version%.apk}
			files=""
			for f in $(cd $d && ls); do
				echo "$installed" | grep -qxF "${f%.apk}" || files="$files $f"
			done
			echo "$p $version$files" >>$cat/tools.txt
			cp -f $d/*.apk $cat/files/
			rm -rf $d
		done
		ok=1
		while read -r p version files; do
			case "$p" in "" | "#"*) continue ;; esac
			rm -rf /tmp/scratch && cp -a $t /tmp/scratch
			args=""
			for f in $files; do args="$args $cat/files/$f"; done
			# shellcheck disable=SC2086 # file names contain no spaces
			if ! log=$(apk --root /tmp/scratch --keys-dir /tmp/scratch/etc/apk/keys --arch aarch64 add --no-network --no-cache --simulate --repositories-file /dev/null $args 2>&1) || echo "$log" | grep -q ERROR; then
				echo "tool $p does not install offline:"
				echo "$log"
				ok=0
			fi
		done <$cat/tools.txt
		rm -rf /tmp/scratch
		[ $ok -eq 1 ] || exit 1
		mib=$(($(du -sm $cat/files | cut -f1) * 5 / 4 + 32))
		ext4 $cat/files /out/claude-code/tools.ext4 "${mib}M" hcc-tools
		cp $cat/tools.txt /out/claude-code/tools.txt
		echo "   tool catalog: $(grep -vc '^#' $cat/tools.txt) packages, $mib MiB"
	fi
	ext4 $t /out/claude-code/rootfs.ext4 1G hcc-claude
	finish claude-code "$c_inputs"
fi
