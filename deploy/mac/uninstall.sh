#!/bin/sh
# Remove the use-my-mac agent. Pass --purge to also delete config,
# credential, work dir, logs and the _umm service user.
set -eu
LABEL="io.github.alee-ibrahim.umm-agent"
PLIST="/Library/LaunchDaemons/${LABEL}.plist"
UPD_PLIST="/Library/LaunchDaemons/${LABEL}.updater.plist"
[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }

launchctl bootout system "$UPD_PLIST" 2>/dev/null || true
launchctl bootout "system/${LABEL}.runtimes" 2>/dev/null || true
rm -f "/Library/LaunchDaemons/${LABEL}.runtimes.plist"
launchctl bootout system "$PLIST" 2>/dev/null || true
rm -f "$UPD_PLIST" "$PLIST" /usr/local/bin/umm-agent /usr/local/bin/.umm-agent.new
rm -rf /usr/local/var/umm-updater /usr/local/var/umm-runtimes
# Work volume (recorded in host.json; versions up to v0.1.8 also named it in
# an fstab line, removed here when /etc/fstab is writable) and the task
# network pf anchor.
FSTAB="/etc/fstab"
UUIDS="$(sed -n 's/^ *"uuid": *"\([0-9A-Fa-f-]*\)".*/\1/p' "/usr/local/etc/umm/host.json" 2>/dev/null || true)"
if [ -f "$FSTAB" ] && grep -q '# use-my-mac work volume$' "$FSTAB"; then
  UUIDS="$UUIDS $(sed -n 's/^UUID=\([0-9A-Fa-f-]*\) .*# use-my-mac work volume$/\1/p' "$FSTAB")"
  sed -i '' '/# use-my-mac work volume$/d' "$FSTAB" 2>/dev/null || true
fi
for u in $(printf '%s\n' $UUIDS | sort -u); do
  diskutil unmount force "$u" >/dev/null 2>&1 || true
  diskutil apfs deleteVolume "$u" >/dev/null 2>&1 || true
done
rm -f "$FSTAB.umm.tmp" 2>/dev/null || true
pfctl -a com.apple/250.use-my-mac -F all >/dev/null 2>&1 || true
if [ "${1:-}" = "--purge" ]; then
  rm -rf /usr/local/etc/umm /usr/local/var/umm /usr/local/var/log/umm /usr/local/var/log/umm-updater.log
  dscl . -delete /Users/_umm 2>/dev/null || true
  dscl . -delete /Groups/_umm 2>/dev/null || true
fi
echo "umm-agent removed. Also remove the Mac in the dashboard so its credential is revoked."
