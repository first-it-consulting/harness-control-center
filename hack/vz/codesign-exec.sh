#!/bin/sh
# go test -exec hack/vz/codesign-exec.sh: signs the test binary ad hoc with
# the virtualization entitlement (ADR-0016), then runs it. Without it,
# Apple's Virtualization framework refuses to start VMs.
set -e
codesign --force --sign - --entitlements "$(dirname "$0")/entitlements.plist" "$1" 2>/dev/null
exec "$@"
