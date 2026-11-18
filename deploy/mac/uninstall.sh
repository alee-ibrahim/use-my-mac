#!/bin/sh
# Remove the use-my-mac agent. Pass --purge to also delete config,
# credential, work dir, logs and the _umm service user.
set -eu
LABEL="io.github.alee-ibrahim.umm-agent"
PLIST="/Library/LaunchDaemons/${LABEL}.plist"
[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }

launchctl bootout system "$PLIST" 2>/dev/null || true
rm -f "$PLIST" /usr/local/bin/umm-agent
if [ "${1:-}" = "--purge" ]; then
  rm -rf /usr/local/etc/umm /usr/local/var/umm /usr/local/var/log/umm
  dscl . -delete /Users/_umm 2>/dev/null || true
  dscl . -delete /Groups/_umm 2>/dev/null || true
fi
echo "umm-agent removed. Also remove the Mac in the dashboard so its credential is revoked."
