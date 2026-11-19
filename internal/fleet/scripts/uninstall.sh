#!/bin/sh
# umm fleet uninstall (root-owned; run via `sudo -n .../uninstall <stage> [--purge]`).
# Stops and removes the agent. With --purge also removes config, credential,
# work dir, logs, the service user, the sudoers drop-in and the managed scripts.
set -eu

STAGE="${1:?usage: uninstall <stage-dir> [--purge]}"
PURGE="${2:-}"
SVC_USER="_umm"
LABEL="$(cat "$STAGE/label" 2>/dev/null || echo io.github.alee-ibrahim.umm-agent)"
PLIST="/Library/LaunchDaemons/${LABEL}.plist"

[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }

launchctl bootout "system/${LABEL}.updater" 2>/dev/null || true
launchctl bootout "system/${LABEL}" 2>/dev/null || true
rm -f "/Library/LaunchDaemons/${LABEL}.updater.plist" "$PLIST" /usr/local/bin/umm-agent /usr/local/bin/.umm-agent.new
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

if [ "$PURGE" = "--purge" ]; then
  rm -rf /usr/local/etc/umm /usr/local/var/umm /usr/local/var/log/umm /usr/local/var/log/umm-updater.log
  dscl . -delete "/Users/${SVC_USER}" 2>/dev/null || true
  dscl . -delete "/Groups/${SVC_USER}" 2>/dev/null || true
  rm -f /etc/sudoers.d/umm
  rm -rf /usr/local/libexec/umm
  echo "purged"
else
  echo "removed (config and credential kept; pass --purge to remove them)"
fi
