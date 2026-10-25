#!/usr/bin/env bash
# Installs the hcc runner daemon on this Linux/KVM host. Run as an admin
# user with sudo; the runner itself runs as the system user hcc-runner,
# whose only root access is the hcc-helper (one sudoers rule). The admin
# user is added to the group hcc, which may use the runner's socket.
#
#   install-runner.sh <hcc-binary> <hcc-helper-binary> <assets-dir> <target>
#
# <assets-dir> holds firecracker, jailer, vmlinux and
# alpine-minirootfs.tar.gz, checksum-verified by hack/firecracker-poc/run.sh.
# <target> is this runner's name as harnesses address it, e.g.
# server:hcc-dev-1.
set -euo pipefail

HCC_BIN=$1
HELPER_BIN=$2
ASSETS=$3
TARGET=$4
ADMIN_USER=$(id -un)
RUN_USER=hcc-runner
GROUP=hcc
HERE=$(cd "$(dirname "$0")" && pwd)
LIB=/usr/local/lib/hcc
IMAGES=$LIB/images

[[ $TARGET =~ ^server:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || { echo "invalid target: $TARGET" >&2; exit 1; }
[[ $(id -u) -ne 0 ]] || { echo "run as an admin user with sudo, not as root" >&2; exit 1; }
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
	for m in proc dev; do sudo umount "$build/cc-tree/$m" 2>/dev/null || true; done
	sudo rm -rf "$build"
}
trap cleanup EXIT

# install_image NAME TREE ROOTFS: kernel and rootfs, root-owned, read-only
# for the runner.
install_image() {
	sudo install -d -o root -g root -m 0755 "$IMAGES/$1"
	sudo install -o root -g root -m 0644 "$ASSETS/vmlinux" "$IMAGES/$1/vmlinux"
	sudo install -o root -g root -m 0644 "$3" "$IMAGES/$1/rootfs.ext4"
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
cc_inputs=$(echo "$CC_VERSION $CC_INTEGRITY" | cat - "$HERE/images/common.sh" "$HERE/images/claude-code/init" "$ASSETS/alpine-minirootfs.tar.gz" "$ASSETS/vmlinux" | sha256sum | cut -d' ' -f1)
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
	# Signed Alpine packages (apk verifies them against the keys in the
	# minirootfs). The chroot gets /proc, /dev and DNS for the build only.
	sudo cp -L /etc/resolv.conf "$tree/etc/resolv.conf"
	sudo mount -t proc proc "$tree/proc"
	sudo mount --bind /dev "$tree/dev"
	sudo chroot "$tree" /sbin/apk add --no-cache bash git jq ripgrep libgcc libstdc++ ca-certificates >/dev/null
	sudo chroot "$tree" /usr/sbin/adduser -D -u 1000 -s /bin/bash -h /home/agent agent
	sudo chroot "$tree" /usr/local/bin/claude --version
	sudo umount "$tree/proc" "$tree/dev"
	sudo rm -f "$tree/etc/resolv.conf"
	sudo mkfs.ext4 -q -F -d "$tree" -L hcc-claude "$build/cc.ext4" 1G
	install_image claude-code "$tree" "$build/cc.ext4"
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
  # Each running VM gets its own IDs: 64000..64000+maxConcurrent-1.
  uid: 64000
  gid: 64000
images:
  base:
    kernel: $IMAGES/base/vmlinux
    rootfs: $IMAGES/base/rootfs.ext4
  claude-code:
    kernel: $IMAGES/claude-code/vmlinux
    rootfs: $IMAGES/claude-code/rootfs.ext4
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
