#!/usr/bin/env bash
# Firecracker proof of concept: boot a minimal, offline microVM under the
# jailer and measure how long it takes. Run on a Linux host with /dev/kvm
# as a normal user with sudo; only the jailer itself runs as root, and it
# immediately drops Firecracker to an unprivileged UID inside a chroot.
#
# Every download is pinned to a version and verified against a SHA-256.
set -euo pipefail

FC_VERSION="v1.17.0"
FC_SHA256="06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558"

# Firecracker CI guest kernel. Upstream publishes no checksum; this hash was
# pinned on first download (2026-10-25). Releases will build their own kernel.
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20261023-dd7c24b0d165-0/x86_64/vmlinux-6.1.188"
KERNEL_SHA256="fc7a33d23a629d64db61ddf12789c737c12edb1702a5ce9d5546b95ca21dc6ca"

ALPINE_VERSION="3.24.3"
ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/x86_64/alpine-minirootfs-${ALPINE_VERSION}-x86_64.tar.gz"
ALPINE_SHA256="f8e1cafba9b1b283b5d30d06f9885c8d2bc161ffe9083ade8e2e157813a53407"

# Unprivileged identity the microVM runs as. It needs no account.
JAIL_UID=64000
JAIL_GID=64000
CHROOT_BASE="/srv/hcc-jailer"
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/hcc-poc"

log() { printf '[%s] %s\n' "$(date +%T)" "$*" >&2; }

fetch() { # url sha256 dest
	local url=$1 sum=$2 dest=$3
	if [[ ! -f $dest ]] || ! echo "$sum  $dest" | sha256sum -c --quiet - 2>/dev/null; then
		log "downloading $(basename "$dest")"
		curl -fsSL --proto '=https' --tlsv1.2 -o "$dest.part" "$url"
		echo "$sum  $dest.part" | sha256sum -c --quiet - || { log "CHECKSUM MISMATCH for $url"; rm -f "$dest.part"; exit 1; }
		mv "$dest.part" "$dest"
	fi
}

preflight() {
	[[ -c /dev/kvm ]] || { log "/dev/kvm missing: enable (nested) virtualization"; exit 1; }
	[[ -r /dev/kvm && -w /dev/kvm ]] || log "note: $(id -un) cannot open /dev/kvm (only the jailer needs it)"
	sudo -n true 2>/dev/null || { log "needs non-interactive sudo for the jailer"; exit 1; }
}

prepare() {
	mkdir -p "$CACHE"
	fetch "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-x86_64.tgz" \
		"$FC_SHA256" "$CACHE/firecracker-${FC_VERSION}.tgz"
	fetch "$KERNEL_URL" "$KERNEL_SHA256" "$CACHE/vmlinux"
	fetch "$ALPINE_URL" "$ALPINE_SHA256" "$CACHE/alpine-minirootfs.tar.gz"

	local rel="release-${FC_VERSION}-x86_64"
	if [[ ! -x $CACHE/firecracker || ! -x $CACHE/jailer ]]; then
		tar -xzf "$CACHE/firecracker-${FC_VERSION}.tgz" -C "$CACHE"
		install -m 0755 "$CACHE/$rel/firecracker-${FC_VERSION}-x86_64" "$CACHE/firecracker"
		install -m 0755 "$CACHE/$rel/jailer-${FC_VERSION}-x86_64" "$CACHE/jailer"
		rm -rf "${CACHE:?}/$rel"
	fi

	# Root filesystem: Alpine minirootfs plus an init that reports and powers
	# off. Built without root via mkfs.ext4 -d; mounted read-only by the guest.
	local tree="$CACHE/rootfs"
	rm -rf "$tree" && mkdir -p "$tree"
	tar -xzf "$CACHE/alpine-minirootfs.tar.gz" -C "$tree"
	cat >"$tree/init" <<-'EOF'
	#!/bin/sh
	mount -t proc proc /proc
	mount -t sysfs sys /sys
	echo "HCC-GUEST-UP kernel=$(uname -r) uid=$(id -u) mem=$(awk '/MemTotal/{print $2" kB"}' /proc/meminfo)"
	echo "HCC-GUEST-NET interfaces: $(ls /sys/class/net | tr '\n' ' ')"
	echo "HCC-GUEST-ROOTFS $(awk '$2=="/"{print $4}' /proc/mounts | cut -d, -f1)"
	sleep 1 # lets the host inspect the running VMM; not part of the boot time
	reboot -f
	EOF
	chmod 0755 "$tree/init"
	rm -f "$CACHE/rootfs.ext4"
	mkfs.ext4 -q -F -d "$tree" -E root_owner=0:0 -L hcc-root "$CACHE/rootfs.ext4" 64M
}

VM_ID=""
cleanup() {
	[[ -n $VM_ID ]] || return 0
	sudo rm -rf "${CHROOT_BASE:?}/firecracker/$VM_ID"
	sudo rmdir "/sys/fs/cgroup/firecracker/$VM_ID" 2>/dev/null || true
}
trap cleanup EXIT

# Show, from the host, how the running VMM is confined.
inspect_vmm() {
	local pid
	# The jailer execs Firecracker in place, so the VMM is the newest
	# process named "firecracker".
	pid=$(pgrep -n -x firecracker || true)
	[[ -n $pid ]] || { log "host: firecracker process not found"; return; }
	log "host: pid $pid uid=$(awk '/^Uid:/{print $2}' /proc/$pid/status) seccomp=$(awk '/^Seccomp:/{print $2}' /proc/$pid/status) (2 = filter) cgroup=$(cut -d: -f3 /proc/$pid/cgroup)"
	# The jailer pivot_roots inside a new mount namespace, so show what the
	# VMM can see instead of a path (readlink would print "/").
	log "host: mount ns $(sudo readlink /proc/$pid/ns/mnt) vs host $(readlink /proc/self/ns/mnt); VMM sees only: $(sudo ls -A /proc/$pid/root | tr '\n' ' ')"
	log "host: cgroup limits: memory.max=$(cat /sys/fs/cgroup/firecracker/$VM_ID/memory.max 2>/dev/null) pids.max=$(cat /sys/fs/cgroup/firecracker/$VM_ID/pids.max 2>/dev/null)"
}

run_vm() {
	VM_ID="poc-$(date +%s)"
	local id=$VM_ID
	local root="$CHROOT_BASE/firecracker/$id/root"

	sudo install -d -m 0750 "$root"
	sudo install -m 0400 -o "$JAIL_UID" -g "$JAIL_GID" "$CACHE/vmlinux" "$root/vmlinux"
	sudo install -m 0400 -o "$JAIL_UID" -g "$JAIL_GID" "$CACHE/rootfs.ext4" "$root/rootfs.ext4"
	sudo tee "$root/config.json" >/dev/null <<-EOF
	{
	  "boot-source": {
	    "kernel_image_path": "vmlinux",
	    "boot_args": "console=ttyS0 quiet loglevel=1 reboot=k panic=1 pci=off ro init=/init"
	  },
	  "drives": [
	    {"drive_id": "rootfs", "path_on_host": "rootfs.ext4", "is_root_device": true, "is_read_only": true}
	  ],
	  "machine-config": {"vcpu_count": 1, "mem_size_mib": 128}
	}
	EOF
	sudo chown "$JAIL_UID:$JAIL_GID" "$root/config.json"

	log "starting microVM $id (uid $JAIL_UID, no network, read-only root)"
	local start end up=""
	start=$(date +%s%N)
	# The jailer chroots, applies cgroup v2 limits, drops to JAIL_UID and
	# execs Firecracker, which installs its seccomp filters before booting.
	while IFS= read -r line; do
		[[ -z $up && $line == HCC-GUEST-UP* ]] && up=$(date +%s%N)
		[[ $line == HCC-* ]] && log "guest: $line"
		[[ $line == HCC-GUEST-ROOTFS* ]] && inspect_vmm
	done < <(sudo "$CACHE/jailer" \
		--id "$id" --exec-file "$CACHE/firecracker" \
		--uid "$JAIL_UID" --gid "$JAIL_GID" \
		--chroot-base-dir "$CHROOT_BASE" \
		--cgroup-version 2 --cgroup memory.max=268435456 --cgroup pids.max=64 \
		-- --no-api --config-file config.json 2>&1)
	end=$(date +%s%N)

	[[ -n $up ]] || { log "FAILED: guest never reported up"; exit 1; }
	log "guest userspace up after $(((up - start) / 1000000)) ms (VM exited after $(((end - start) / 1000000)) ms, incl. the 1 s inspection pause)"
}

preflight
prepare
run_vm
