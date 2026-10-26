# Shared by the image inits (/usr/lib/hcc/common.sh in every image).

# The kernel starts init with a minimal PATH.
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin

hcc_arg() { for kv in $(cat /proc/cmdline); do case "$kv" in "$1="*) echo "${kv#*=}" ;; esac; done; }

# The root filesystem is read-only; /tmp is memory, gone with the VM.
hcc_mounts() {
	mount -t proc proc /proc
	mount -t sysfs sys /sys
	mount -t devtmpfs dev /dev 2>/dev/null || true
	# What a full system's device manager would add.
	ln -sf /proc/self/fd /dev/fd
	ln -sf /proc/self/fd/0 /dev/stdin
	ln -sf /proc/self/fd/1 /dev/stdout
	ln -sf /proc/self/fd/2 /dev/stderr
	mount -t tmpfs -o mode=1777,size=64m tmpfs /tmp
}

# If the harness grants network access: eth0 with an address but no
# gateway and no DNS (the egress proxy is on-link and resolves names), the
# job CA (only for jobs with https credentials) and the proxy variables.
hcc_network() {
	HCC_NET=$(hcc_arg hcc.ip)
	HCC_PROXY=$(hcc_arg hcc.proxy)
	[ -n "$HCC_NET" ] || return 0
	ip link set lo up
	ip addr add "$HCC_NET" dev eth0
	ip link set eth0 up
	if wget -q -T 5 -O /tmp/hcc-ca.pem "http://$HCC_PROXY/hcc/ca.pem" 2>/dev/null && grep -q "BEGIN CERTIFICATE" /tmp/hcc-ca.pem; then
		cat /etc/ssl/certs/ca-certificates.crt /tmp/hcc-ca.pem >/tmp/hcc-bundle.pem 2>/dev/null || cp /tmp/hcc-ca.pem /tmp/hcc-bundle.pem
		export SSL_CERT_FILE=/tmp/hcc-bundle.pem NODE_EXTRA_CA_CERTS=/tmp/hcc-ca.pem REQUESTS_CA_BUNDLE=/tmp/hcc-bundle.pem \
			CURL_CA_BUNDLE=/tmp/hcc-bundle.pem GIT_SSL_CAINFO=/tmp/hcc-bundle.pem
	else
		rm -f /tmp/hcc-ca.pem
	fi
	export HTTPS_PROXY="http://$HCC_PROXY" https_proxy="http://$HCC_PROXY" HTTP_PROXY="http://$HCC_PROXY" http_proxy="http://$HCC_PROXY"
}

# Installs the task's tools (ADR-0010) before the agent starts, as root,
# from the read-only tool catalog drive (hcc.tools) onto memory overlays
# over the read-only system directories (nosuid: no package can hand the
# agent root through a setuid binary). apk verifies every package's
# signature against the image's keys, and there is no repository to
# fetch anything else from. The drive is unmounted afterwards; its device
# is root's, so the agent cannot read the catalog. Needs jq. Returns
# non-zero if anything failed: then nothing may run.
hcc_tools() {
	task=$1
	files=$(echo "$task" | jq -r '.tools.files[]? // empty') || return 1
	[ -n "$files" ] || return 0
	dev=$(hcc_arg hcc.tools)
	case "$dev" in
	vd[a-z]) ;;
	*) echo "HCC-TOOLS-ERROR no tool catalog attached"; return 1 ;;
	esac
	set --
	for f in $files; do
		case "$f" in
		*[!A-Za-z0-9._+-]*) echo "HCC-TOOLS-ERROR invalid file name"; return 1 ;;
		[A-Za-z0-9]*.apk) set -- "$@" "/tmp/hcc-tools/$f" ;;
		*) echo "HCC-TOOLS-ERROR invalid file name"; return 1 ;;
		esac
	done
	mkdir -m 0700 /tmp/hcc-ov /tmp/hcc-tools || return 1
	mount -t tmpfs -o mode=0700,size=50% tmpfs /tmp/hcc-ov || return 1
	for d in bin sbin lib usr etc var; do
		mkdir -p "/tmp/hcc-ov/$d/upper" "/tmp/hcc-ov/$d/work" &&
			mount -t overlay overlay -o "nosuid,nodev,lowerdir=/$d,upperdir=/tmp/hcc-ov/$d/upper,workdir=/tmp/hcc-ov/$d/work" "/$d" ||
			{ echo "HCC-TOOLS-ERROR cannot make /$d writable"; return 1; }
	done
	mount -t ext4 -o ro,nodev,nosuid,noexec "/dev/$dev" /tmp/hcc-tools || { echo "HCC-TOOLS-ERROR cannot mount the catalog"; return 1; }
	apk add --no-network --no-cache --quiet --repositories-file /dev/null "$@" >/tmp/hcc-apk.log 2>&1
	rc=$?
	umount /tmp/hcc-tools && rmdir /tmp/hcc-tools || { echo "HCC-TOOLS-ERROR cannot unmount the catalog"; return 1; }
	if [ "$rc" -ne 0 ]; then
		echo "HCC-TOOLS-ERROR apk failed: $(tail -n 3 /tmp/hcc-apk.log | tr -d '\000-\037')"
		return 1
	fi
	echo "HCC-TOOLS installed: $(echo "$task" | jq -r '.tools.packages | join(" ")')"
}

# The task (JSON, read-only drive vdb), zero padding removed.
hcc_task() { [ -b /dev/vdb ] && tr -d '\000' </dev/vdb; }

# The writable disk (spec.sandbox.diskMiB), if the runner attached one:
# the only virtio disk that is not read-only.
hcc_disk() {
	for d in /sys/block/vd*; do
		[ -e "$d/ro" ] && [ "$(cat "$d/ro")" = 0 ] && { echo "/dev/${d##*/}"; return 0; }
	done
	return 1
}

# Hands a directory back as the job's results: a tar archive on the disk,
# which the runner reads and sanitizes after shutdown (it keeps only
# regular files and directories). Must run before shutdown; "reboot -f"
# does not sync.
hcc_save_results() {
	dev=$(hcc_disk) || { echo "HCC-RESULTS none (no disk)"; return 0; }
	if (cd "$1" && tar -cf "$dev" .) 2>/tmp/hcc-tar.err; then
		echo "HCC-RESULTS saved"
	else
		echo "HCC-RESULTS failed: $(head -c 200 /tmp/hcc-tar.err | tr -d '\000-\037')"
	fi
	sync
}
