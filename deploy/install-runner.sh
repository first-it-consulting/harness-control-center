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
for tool in /usr/sbin/ip /usr/sbin/nft; do
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
sudo install -d -o root -g root -m 0755 "$LIB" "$IMAGES" "$IMAGES/base"
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

echo "== image 'base' (read-only for the runner)"
build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT
mkdir "$build/tree"
tar -xzf "$ASSETS/alpine-minirootfs.tar.gz" -C "$build/tree"
cat >"$build/tree/init" <<'INIT'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
arg() { for kv in $(cat /proc/cmdline); do case "$kv" in "$1="*) echo "${kv#*=}" ;; esac; done; }
echo "HCC-GUEST-UP kernel=$(uname -r)"
NET=$(arg hcc.ip)
PROXY=$(arg hcc.proxy)
if [ -n "$NET" ]; then
	# Address only, no gateway: the proxy is on-link and nothing else is
	# reachable. No DNS either: the proxy resolves names.
	ip link set lo up
	ip addr add "$NET" dev eth0
	ip link set eth0 up
	# Jobs with credentials: trust the job's CA (public; fetched directly
	# from the proxy, before the proxy variables exist).
	mount -t tmpfs -o mode=1777,size=16m tmpfs /tmp
	if wget -q -T 5 -O /tmp/hcc-ca.pem "http://$PROXY/hcc/ca.pem" 2>/dev/null && grep -q "BEGIN CERTIFICATE" /tmp/hcc-ca.pem; then
		cat /etc/ssl/certs/ca-certificates.crt /tmp/hcc-ca.pem >/tmp/hcc-bundle.pem 2>/dev/null || cp /tmp/hcc-ca.pem /tmp/hcc-bundle.pem
		export SSL_CERT_FILE=/tmp/hcc-bundle.pem NODE_EXTRA_CA_CERTS=/tmp/hcc-ca.pem REQUESTS_CA_BUNDLE=/tmp/hcc-bundle.pem \
			CURL_CA_BUNDLE=/tmp/hcc-bundle.pem GIT_SSL_CAINFO=/tmp/hcc-bundle.pem
	else
		rm -f /tmp/hcc-ca.pem
	fi
	export HTTPS_PROXY="http://$PROXY" https_proxy="http://$PROXY" HTTP_PROXY="http://$PROXY" http_proxy="http://$PROXY"
fi
echo "HCC-GUEST-NET interfaces: $(ls /sys/class/net | tr '\n' ' ')"
# Test modes for stopping, the kill switch and the network guard only;
# harnesses cannot set kernel arguments.
case " $(cat /proc/cmdline) " in
*" hcc.mode=hang "*) echo "HCC-GUEST-HANG"; while :; do sleep 1; done ;;
*" hcc.mode=nettest "*)
	H=${PROXY%:*}
	P=${PROXY##*:}
	probe() { printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n' "$1" "$1" | nc -w 3 "$H" "$P" 2>/dev/null | head -n 1 | tr -d '\r'; }
	echo "HCC-NET-ALLOWED $(probe example.com:443)"
	echo "HCC-NET-DENIED $(probe example.org:443)"
	if nc -w 2 1.1.1.1 443 </dev/null 2>/dev/null; then echo "HCC-NET-DIRECT open"; else echo "HCC-NET-DIRECT blocked"; fi
	if nc -w 2 "$H" 22 </dev/null 2>/dev/null; then echo "HCC-NET-HOSTPORT open"; else echo "HCC-NET-HOSTPORT blocked"; fi
	if [ -s /tmp/hcc-ca.pem ]; then echo "HCC-NET-CA present"; else echo "HCC-NET-CA absent"; fi
	;;
esac
echo "HCC-GUEST-DONE no agent runtime yet; shutting down"
reboot -f
INIT
chmod 0755 "$build/tree/init"
mkfs.ext4 -q -F -d "$build/tree" -E root_owner=0:0 -L hcc-base "$build/rootfs.ext4" 64M
sudo install -o root -g root -m 0644 "$ASSETS/vmlinux" "$IMAGES/base/vmlinux"
sudo install -o root -g root -m 0644 "$build/rootfs.ext4" "$IMAGES/base/rootfs.ext4"

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
