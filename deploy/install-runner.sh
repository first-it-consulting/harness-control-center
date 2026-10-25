#!/usr/bin/env bash
# Installs the hcc runner daemon on this Linux/KVM host. Run as the user
# the runner should run as (it needs non-interactive sudo for the jailer).
#
#   install-runner.sh <hcc-binary> <assets-dir> <target>
#
# <assets-dir> holds firecracker, jailer, vmlinux and
# alpine-minirootfs.tar.gz, checksum-verified by hack/firecracker-poc/run.sh.
# <target> is this runner's name as harnesses address it, e.g.
# server:hcc-dev-1.
set -euo pipefail

HCC_BIN=$1
ASSETS=$2
TARGET=$3
RUN_USER=$(id -un)
HERE=$(cd "$(dirname "$0")" && pwd)
LIB=/usr/local/lib/hcc
IMAGES=$LIB/images

[[ $TARGET =~ ^server:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || { echo "invalid target: $TARGET" >&2; exit 1; }
[[ $(id -u) -ne 0 ]] || { echo "run as the unprivileged runner user, not root" >&2; exit 1; }
for f in firecracker jailer vmlinux alpine-minirootfs.tar.gz; do
	[[ -f $ASSETS/$f ]] || { echo "missing $ASSETS/$f (run hack/firecracker-poc/run.sh first)" >&2; exit 1; }
done

echo "== binaries (root-owned: the jailer runs as root)"
sudo install -o root -g root -m 0755 "$HCC_BIN" /usr/local/bin/hcc
sudo install -d -o root -g root -m 0755 "$LIB" "$IMAGES" "$IMAGES/base"
sudo install -o root -g root -m 0755 "$ASSETS/firecracker" "$ASSETS/jailer" "$LIB/"

echo "== image 'base' (read-only for the runner)"
build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT
mkdir "$build/tree"
tar -xzf "$ASSETS/alpine-minirootfs.tar.gz" -C "$build/tree"
cat >"$build/tree/init" <<'INIT'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
echo "HCC-GUEST-UP kernel=$(uname -r)"
echo "HCC-GUEST-NET interfaces: $(ls /sys/class/net | tr '\n' ' ')"
echo "HCC-GUEST-DONE no agent runtime yet; shutting down"
reboot -f
INIT
chmod 0755 "$build/tree/init"
mkfs.ext4 -q -F -d "$build/tree" -E root_owner=0:0 -L hcc-base "$build/rootfs.ext4" 64M
sudo install -o root -g root -m 0644 "$ASSETS/vmlinux" "$IMAGES/base/vmlinux"
sudo install -o root -g root -m 0644 "$build/rootfs.ext4" "$IMAGES/base/rootfs.ext4"

echo "== configuration"
sudo install -d -o root -g root -m 0755 /etc/hcc
sudo tee /etc/hcc/runner.yaml >/dev/null <<CONF
# Managed by install-runner.sh.
target: $TARGET
maxConcurrent: 2
socket: /run/hcc/runner.sock
stateDir: /var/lib/hcc/state
firecracker:
  jailer: $LIB/jailer
  firecracker: $LIB/firecracker
  chrootBase: /var/lib/hcc/jails
  imageRoot: $IMAGES
  # Each running VM gets its own IDs: 64000..64000+maxConcurrent-1.
  uid: 64000
  gid: 64000
images:
  base:
    kernel: $IMAGES/base/vmlinux
    rootfs: $IMAGES/base/rootfs.ext4
CONF
sudo chmod 0644 /etc/hcc/runner.yaml

echo "== systemd unit (User=$RUN_USER)"
sed "s/@RUN_USER@/$RUN_USER/" "$HERE/hcc-runner.service" | sudo tee /etc/systemd/system/hcc-runner.service >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable --quiet hcc-runner
sudo systemctl restart hcc-runner
sleep 1
systemctl is-active --quiet hcc-runner || { sudo journalctl -u hcc-runner -n 30 --no-pager; exit 1; }
echo "hcc runner is active ($TARGET), socket /run/hcc/runner.sock"
