#!/bin/sh
# Remove the use-my-mac agent. Pass --purge to also delete config,
# credential, work dir, logs and the _umm service user.
set -eu
LABEL="io.github.alee-ibrahim.umm-agent"
PLIST="/Library/LaunchDaemons/${LABEL}.plist"
UPD_PLIST="/Library/LaunchDaemons/${LABEL}.updater.plist"
[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }

launchctl bootout system "$UPD_PLIST" 2>/dev/null || true
launchctl bootout system "$PLIST" 2>/dev/null || true
rm -f "$UPD_PLIST" "$PLIST" /usr/local/bin/umm-agent /usr/local/bin/.umm-agent.new
rm -rf /usr/local/var/umm-updater
# Work volume (its fstab line names it) and the task network pf anchor.
FSTAB="/etc/fstab"
if [ -f "$FSTAB" ] && grep -q '# use-my-mac work volume$' "$FSTAB"; then
  for u in $(sed -n 's/^UUID=\([0-9A-Fa-f-]*\) .*# use-my-mac work volume$/\1/p' "$FSTAB"); do
    diskutil unmount force "$u" >/dev/null 2>&1 || true
    diskutil apfs deleteVolume "$u" >/dev/null 2>&1 || true
  done
  sed -i '' '/# use-my-mac work volume$/d' "$FSTAB" 2>/dev/null || true
fi
pfctl -a com.apple/250.use-my-mac -F all >/dev/null 2>&1 || true
if [ "${1:-}" = "--purge" ]; then
  rm -rf /usr/local/etc/umm /usr/local/var/umm /usr/local/var/log/umm /usr/local/var/log/umm-updater.log
  dscl . -delete /Users/_umm 2>/dev/null || true
  dscl . -delete /Groups/_umm 2>/dev/null || true
fi
echo "umm-agent removed. Also remove the Mac in the dashboard so its credential is revoked."
