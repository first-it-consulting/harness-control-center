#!/bin/sh
# Posts each event (one JSON object per line on stdin) to the URL in $1.
# Event messages may come from agents: the receiver must treat them as
# untrusted text. Through the sandbox's proxy only: plain http URLs use a
# CONNECT tunnel (-p).
set -eu
url=$1
failed=0
while IFS= read -r event; do
	printf '%s' "$event" | curl -sS -p --max-time 20 -H 'Content-Type: application/json' \
		--data-binary @- -o /dev/null -w '%{http_code}\n' "$url" || failed=1
done
exit "$failed"
