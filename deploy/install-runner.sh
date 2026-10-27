#!/usr/bin/env bash
# Installs the hcc runner daemon on this Linux/KVM host. Run as an admin
# user with sudo; the runner itself runs as the system user hcc-runner,
# whose only root access is the hcc-helper (one sudoers rule). The admin
# user is added to the group hcc, which may use the runner's socket.
#
#   install-runner.sh <hcc-binary> <hcc-helper-binary> <hcc-guest-binary> <assets-dir> <target>
#
# <assets-dir> holds firecracker, jailer, vmlinux and
# alpine-minirootfs.tar.gz. Missing pinned assets are fetched and verified.
# <target> is this runner's name as harnesses address it, e.g.
# server:hcc-dev.
#
# HCC_TOOLS lists the Alpine packages the claude-code image offers as
# tools (ADR-0010); set it to "" for none.
set -euo pipefail

HCC_BIN=$1
HELPER_BIN=$2
GUEST_BIN=$3
ASSETS=$4
TARGET=$5
ADMIN_USER=$(id -un)
TOOLS=${HCC_TOOLS-python3 py3-pip nodejs npm make gcc musl-dev curl go}
RUN_USER=hcc-runner
GROUP=hcc
HERE=$(cd "$(dirname "$0")" && pwd)
LIB=/usr/local/lib/hcc
IMAGES=$LIB/images

[[ $TARGET =~ ^server:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || { echo "invalid target: $TARGET" >&2; exit 1; }
[[ $(id -u) -ne 0 ]] || { echo "run as an admin user with sudo, not as root" >&2; exit 1; }

# Installed from the Control Center with the user's sudo password: sudo
# remembers a password per terminal, or without one per calling process,
# so it would not reach this script's own sudo calls. The Control Center
# then sets SUDO_ASKPASS to a helper that hands sudo the password, and
# every sudo here asks it (-A).
if [[ -n ${SUDO_ASKPASS:-} ]]; then
	sudo() { command sudo -A "$@"; }
fi

# Pinned runner assets. When they are missing (the usual web-UI install),
# fetch and verify them here; when install-runner.sh is called by
# hack/firecracker-poc/run.sh, its already checked files are reused.
FC_VERSION="v1.17.0"
FC_SHA256="06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558"
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20261023-dd7c24b0d165-0/x86_64/vmlinux-6.1.188"
KERNEL_SHA256="fc7a33d23a629d64db61ddf12789c737c12edb1702a5ce9d5546b95ca21dc6ca"
ALPINE_VERSION="3.24.3"
ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/x86_64/alpine-minirootfs-${ALPINE_VERSION}-x86_64.tar.gz"
ALPINE_SHA256="f8e1cafba9b1b283b5d30d06f9885c8d2bc161ffe9083ade8e2e157813a53407"

fetch_asset() { # url sha256 dest
	local url=$1 sum=$2 dest=$3
	if [[ -f $dest ]] && echo "$sum  $dest" | sha256sum -c --quiet - 2>/dev/null; then
		return 0
	fi
	echo "== fetching $(basename "$dest")"
	curl -fsSL --proto '=https' --tlsv1.2 -o "$dest.part" "$url"
	echo "$sum  $dest.part" | sha256sum -c --quiet - || { echo "CHECKSUM MISMATCH for $url" >&2; rm -f "$dest.part"; exit 1; }
	mv "$dest.part" "$dest"
}

[[ $(uname -m) == x86_64 ]] || { echo "runner installation currently supports Linux x86_64 hosts, not $(uname -m)" >&2; exit 1; }
for tool in curl sha256sum tar; do
	command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 1; }
done
mkdir -p "$ASSETS"
if [[ ! -x $ASSETS/firecracker || ! -x $ASSETS/jailer ]]; then
	fetch_asset "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-x86_64.tgz" \
		"$FC_SHA256" "$ASSETS/firecracker-${FC_VERSION}.tgz"
	tar -xzf "$ASSETS/firecracker-${FC_VERSION}.tgz" -C "$ASSETS"
	install -m 0755 "$ASSETS/release-${FC_VERSION}-x86_64/firecracker-${FC_VERSION}-x86_64" "$ASSETS/firecracker"
	install -m 0755 "$ASSETS/release-${FC_VERSION}-x86_64/jailer-${FC_VERSION}-x86_64" "$ASSETS/jailer"
	rm -rf "$ASSETS/release-${FC_VERSION}-x86_64"
fi
fetch_asset "$KERNEL_URL" "$KERNEL_SHA256" "$ASSETS/vmlinux"
fetch_asset "$ALPINE_URL" "$ALPINE_SHA256" "$ASSETS/alpine-minirootfs.tar.gz"

for p in $TOOLS; do
	[[ $p =~ ^[a-z0-9][a-z0-9._+-]{0,127}$ ]] || { echo "invalid tool package name: $p" >&2; exit 1; }
done
for tool in /usr/sbin/ip /usr/sbin/nft /usr/bin/openssl; do
	[[ -x $tool ]] || { echo "missing $tool (iproute2, nftables)" >&2; exit 1; }
done
for f in firecracker jailer vmlinux alpine-minirootfs.tar.gz; do
	[[ -f $ASSETS/$f ]] || { echo "missing $ASSETS/$f (run hack/firecracker-poc/run.sh first)" >&2; exit 1; }
done

echo "== users: $RUN_USER (system, no login), group $GROUP (socket access)"
getent group "$GROUP" >/dev/null || sudo groupadd --system "$GROUP"
if ! id "$RUN_USER" >/dev/null 2>&1; then
	sudo useradd --system --user-group --home-dir /var/lib/hcc --no-create-home \
		--shell /usr/sbin/nologin --comment "hcc runner" "$RUN_USER"
fi
sudo usermod -aG "$GROUP" "$RUN_USER"
sudo usermod -aG "$GROUP" "$ADMIN_USER"

echo "== binaries (root-owned: the helper and the jailer run as root)"
sudo install -o root -g root -m 0755 "$HCC_BIN" /usr/local/bin/hcc
sudo install -d -o root -g root -m 0755 "$LIB" "$IMAGES"
sudo install -o root -g root -m 0755 "$ASSETS/firecracker" "$ASSETS/jailer" "$LIB/"
sudo install -o root -g root -m 0755 "$HELPER_BIN" "$LIB/hcc-helper"
# The jail tree is root's (hcc-helper builds every jail); 0711 lets the
# runner check whether a jail still exists without listing or changing it.
sudo install -d -o root -g root -m 0711 /var/lib/hcc-jails

echo "== sudoers: $RUN_USER may run exactly $LIB/hcc-helper as root"
rule=$(mktemp)
cat >"$rule" <<SUDO
# Managed by install-runner.sh. The runner's only root access: hcc-helper
# validates every argument and builds the jailer command from the
# root-owned /etc/hcc/runner.yaml.
Defaults:$RUN_USER !requiretty
$RUN_USER ALL=(root) NOPASSWD: $LIB/hcc-helper
SUDO
sudo visudo -cf "$rule" >/dev/null
sudo install -o root -g root -m 0440 "$rule" /etc/sudoers.d/hcc-runner
rm -f "$rule"

# Claude Code's native musl build, from the npm registry, checked against
# the registry's sha512 integrity value.
CC_VERSION=2.1.310
CC_INTEGRITY="sha512-NuXHtNyJMa59njKJBHSIY8oVxDYydVaWsa+Ij7VV0bLMSjWjK2L76WG6OhQmJjwGqjOVKd542DhPVDsrX+iB+g=="
CC_URL="https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64-musl/-/claude-code-linux-x64-musl-${CC_VERSION}.tgz"

build=$(mktemp -d)
cleanup() {
	for m in catalog/test/root/dev catalog/test/root/hcc-catalog catalog/test/root cc-tree/proc cc-tree/dev; do sudo umount "$build/$m" 2>/dev/null || true; done
	sudo rm -rf --one-file-system "$build"
}
trap cleanup EXIT

# install_image NAME TREE ROOTFS: kernel and rootfs, root-owned, read-only
# for the runner.
install_image() {
	sudo install -d -o root -g root -m 0755 "$IMAGES/$1"
	sudo install -o root -g root -m 0644 "$ASSETS/vmlinux" "$IMAGES/$1/vmlinux"
	sudo install -o root -g root -m 0644 "$3" "$IMAGES/$1/rootfs.ext4"
}

# build_catalog TREE OUT: the tool catalog of the image in TREE (whose
# chroot has /proc, /dev and DNS): for each package in $TOOLS, its signed
# package file and those of the dependencies the image lacks, fetched now
# from the image's own Alpine release. OUT gets tools.ext4 (the files,
# for the guest) and tools.txt (the package list, for the runner). Every
# package is test-installed offline, from the files alone, in a throwaway
# copy of the image, just as the guest will.
build_catalog() {
	local tree=$1 out=$2 installed p dir own version files
	mkdir -p "$out/files"
	installed=$(sudo chroot "$tree" /sbin/apk info -v 2>/dev/null)
	echo "# name version file... (built by install-runner.sh)" >"$out/tools.txt"
	for p in $TOOLS; do
		dir=hcc-fetch-$p
		sudo mkdir "$tree/$dir"
		sudo chroot "$tree" /sbin/apk fetch --no-cache --recursive --quiet -o "/$dir" "$p"
		own=$(cd "$tree/$dir" && ls | grep -E "^${p//+/\\+}-[0-9][^-]*-r[0-9]+\.apk\$" || true)
		[[ $(wc -w <<<"$own") -eq 1 ]] || { echo "tool $p: cannot tell its package file" >&2; exit 1; }
		version=${own#"$p"-}
		version=${version%.apk}
		files=""
		for f in $(cd "$tree/$dir" && ls); do
			grep -qxF "${f%.apk}" <<<"$installed" || files="$files $f"
		done
		echo "$p $version$files" >>"$out/tools.txt"
		sudo cp -f "$tree/$dir"/*.apk "$out/files/" # same name, same signed package
		sudo rm -rf "$tree/$dir"
	done
	# The test install: the image's apk, its keys, no network, no
	# repository. Overlays keep the image itself unchanged.
	local scratch=$out/test
	mkdir -p "$scratch/upper" "$scratch/work" "$scratch/root"
	sudo mount -t overlay overlay -o "lowerdir=$tree,upperdir=$scratch/upper,workdir=$scratch/work" "$scratch/root"
	sudo mkdir -p "$scratch/root/hcc-catalog"
	sudo mount --bind -o ro "$out/files" "$scratch/root/hcc-catalog"
	sudo mount --bind /dev "$scratch/root/dev"
	local ok=1
	while read -r p version files; do
		[[ -z $p || $p == "#"* ]] && continue
		# shellcheck disable=SC2086 # file names are checked by the runner and contain no spaces
		set -- $files
		# apk reports some problems without failing: any ERROR counts.
		if ! log=$(sudo chroot "$scratch/root" /sbin/apk add --no-network --no-cache --simulate \
			--repositories-file /dev/null "${@/#//hcc-catalog/}" 2>&1) || grep -q ERROR <<<"$log"; then
			echo "tool $p does not install offline:" >&2
			echo "$log" >&2
			ok=0
		fi
	done <"$out/tools.txt"
	sudo umount "$scratch/root/dev" "$scratch/root/hcc-catalog" "$scratch/root"
	sudo rm -rf "$scratch"
	[[ $ok -eq 1 ]] || exit 1
	local mib
	mib=$(( $(sudo du -sm "$out/files" | cut -f1) * 5 / 4 + 32 ))
	sudo mkfs.ext4 -q -F -d "$out/files" -E root_owner=0:0 -L hcc-tools "$out/tools.ext4" "${mib}M"
	echo "   tool catalog: $(grep -vc '^#' "$out/tools.txt") packages, $mib MiB"
}

# Images are rebuilt only when their inputs change.
stamp() { cat "$@" | sha256sum | cut -d' ' -f1; }
uptodate() { sudo test -f "$IMAGES/$1/rootfs.ext4" && [ "$(sudo cat "$IMAGES/$1/.inputs" 2>/dev/null)" = "$2" ]; }
record() { echo "$2" | sudo tee "$IMAGES/$1/.inputs" >/dev/null; }

echo "== image 'base'"
base_inputs=$(stamp "$HERE/images/common.sh" "$HERE/images/base/init" "$ASSETS/alpine-minirootfs.tar.gz" "$ASSETS/vmlinux")
if uptodate base "$base_inputs"; then
	echo "   up to date"
else
	mkdir "$build/tree"
	tar -xzf "$ASSETS/alpine-minirootfs.tar.gz" -C "$build/tree"
	install -D -m 0644 "$HERE/images/common.sh" "$build/tree/usr/lib/hcc/common.sh"
	install -m 0755 "$HERE/images/base/init" "$build/tree/init"
	mkfs.ext4 -q -F -d "$build/tree" -E root_owner=0:0 -L hcc-base "$build/base.ext4" 64M
	install_image base "$build/tree" "$build/base.ext4"
	record base "$base_inputs"
fi

echo "== image 'claude-code' (Claude Code $CC_VERSION)"
cc_inputs=$(echo "$CC_VERSION $CC_INTEGRITY tools: $TOOLS" | cat - "$0" "$HERE/images/common.sh" "$HERE/images/claude-code/init" "$HERE/images/claude-code/hcc-subagent" "$GUEST_BIN" "$ASSETS/alpine-minirootfs.tar.gz" "$ASSETS/vmlinux" | sha256sum | cut -d' ' -f1)
if uptodate claude-code "$cc_inputs"; then
	echo "   up to date"
else
	curl -fsSL --proto '=https' --tlsv1.2 -o "$build/cc.tgz" "$CC_URL"
	got="sha512-$(openssl dgst -sha512 -binary "$build/cc.tgz" | base64 -w0)"
	[[ $got == "$CC_INTEGRITY" ]] || { echo "Claude Code integrity mismatch: $got" >&2; exit 1; }
	tar -xzf "$build/cc.tgz" -C "$build" package/claude
	tree=$build/cc-tree
	sudo mkdir "$tree"
	sudo tar -xzf "$ASSETS/alpine-minirootfs.tar.gz" -C "$tree"
	sudo install -m 0755 "$build/package/claude" "$tree/usr/local/bin/claude"
	sudo install -D -m 0644 "$HERE/images/common.sh" "$tree/usr/lib/hcc/common.sh"
	sudo install -m 0755 "$HERE/images/claude-code/init" "$tree/init"
	sudo install -m 0755 "$HERE/images/claude-code/hcc-subagent" "$tree/usr/local/bin/hcc-subagent"
	sudo install -m 0755 "$GUEST_BIN" "$tree/usr/local/bin/hcc-guest"
	# Signed Alpine packages (apk verifies them against the keys in the
	# minirootfs). The chroot gets /proc, /dev and DNS for the build only.
	sudo cp -L /etc/resolv.conf "$tree/etc/resolv.conf"
	sudo mount -t proc proc "$tree/proc"
	sudo mount --bind /dev "$tree/dev"
	sudo chroot "$tree" /sbin/apk add --no-cache bash git jq ripgrep libgcc libstdc++ ca-certificates >/dev/null
	sudo chroot "$tree" /usr/sbin/adduser -D -u 1000 -s /bin/bash -h /home/agent agent
	sudo chroot "$tree" /usr/local/bin/claude --version
	if [[ -n $TOOLS ]]; then
		build_catalog "$tree" "$build/catalog"
	fi
	sudo umount "$tree/proc" "$tree/dev"
	sudo rm -f "$tree/etc/resolv.conf"
	sudo mkfs.ext4 -q -F -d "$tree" -L hcc-claude "$build/cc.ext4" 1G
	install_image claude-code "$tree" "$build/cc.ext4"
	sudo rm -f "$IMAGES/claude-code/tools.ext4" "$IMAGES/claude-code/tools.txt"
	if [[ -n $TOOLS ]]; then
		sudo install -o root -g root -m 0644 "$build/catalog/tools.ext4" "$build/catalog/tools.txt" "$IMAGES/claude-code/"
	fi
	record claude-code "$cc_inputs"
fi

echo "== configuration"
sudo install -d -o root -g root -m 0755 /etc/hcc
# Credential store: root-owned, readable by the runner only. Keep existing.
sudo install -d -o root -g "$RUN_USER" -m 0750 /etc/hcc/secrets
if ! sudo test -e /etc/hcc/credentials.yaml; then
	printf '%s\n' "# Credential broker store; see deploy/README.md." "{}" | sudo tee /etc/hcc/credentials.yaml >/dev/null
fi
sudo chown root:"$RUN_USER" /etc/hcc/credentials.yaml
sudo chmod 0640 /etc/hcc/credentials.yaml
sudo tee /etc/hcc/runner.yaml >/dev/null <<CONF
# Managed by install-runner.sh.
target: $TARGET
maxConcurrent: 2
# Plugin sandboxes running at once, besides the jobs (ADR-0013).
maxPluginVMs: 2
runnerUser: $RUN_USER
socket: /run/hcc/runner.sock
socketGroup: $GROUP
stateDir: /var/lib/hcc/state
firecracker:
  helper: $LIB/hcc-helper
  jailer: $LIB/jailer
  firecracker: $LIB/firecracker
  chrootBase: /var/lib/hcc-jails
  stagingDir: /var/lib/hcc/staging
  imageRoot: $IMAGES
  # Each running VM gets its own IDs: 64000..64000+maxConcurrent+maxPluginVMs-1.
  uid: 64000
  gid: 64000
images:
  base:
    kernel: $IMAGES/base/vmlinux
    rootfs: $IMAGES/base/rootfs.ext4
  claude-code:
    kernel: $IMAGES/claude-code/vmlinux
    rootfs: $IMAGES/claude-code/rootfs.ext4
$(if sudo test -f "$IMAGES/claude-code/tools.txt"; then printf '    tools:\n      drive: %s\n      packages: %s\n' "$IMAGES/claude-code/tools.ext4" "$IMAGES/claude-code/tools.txt"; fi)
# Credential broker store (root:$RUN_USER 0640).
credentials: /etc/hcc/credentials.yaml
# Sandbox links: one /30 per VM; must not overlap any real network.
network:
  subnet: 172.31.0.0/16
  proxyPort: 3128
CONF
sudo chmod 0644 /etc/hcc/runner.yaml

echo "== systemd unit (User=$RUN_USER)"
sed -e "s/@RUN_USER@/$RUN_USER/" -e "s/@GROUP@/$GROUP/" "$HERE/hcc-runner.service" | sudo tee /etc/systemd/system/hcc-runner.service >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable --quiet hcc-runner
sudo systemctl restart hcc-runner
sleep 1
systemctl is-active --quiet hcc-runner || { sudo journalctl -u hcc-runner -n 30 --no-pager; exit 1; }
echo "hcc runner is active ($TARGET) as $RUN_USER, socket /run/hcc/runner.sock (group $GROUP)"
echo "note: $ADMIN_USER was added to group $GROUP; new ssh sessions pick that up"
