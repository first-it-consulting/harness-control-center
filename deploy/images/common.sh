# Shared by the image inits (/usr/lib/hcc/common.sh in every image).

# The kernel starts init with a minimal PATH.
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin

hcc_arg() { for kv in $(cat /proc/cmdline); do case "$kv" in "$1="*) echo "${kv#*=}" ;; esac; done; }

# The root filesystem is read-only; /tmp is memory, gone with the VM.
hcc_mounts() {
	mount -t proc proc /proc
	mount -t sysfs sys /sys
	mount -t devtmpfs dev /dev 2>/dev/null || true
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
