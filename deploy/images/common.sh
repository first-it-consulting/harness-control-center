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
