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

launchctl bootout "system/${LABEL}" 2>/dev/null || true
rm -f "$PLIST" /usr/local/bin/umm-agent

if [ "$PURGE" = "--purge" ]; then
  rm -rf /usr/local/etc/umm /usr/local/var/umm /usr/local/var/log/umm
  dscl . -delete "/Users/${SVC_USER}" 2>/dev/null || true
  dscl . -delete "/Groups/${SVC_USER}" 2>/dev/null || true
  rm -f /etc/sudoers.d/umm
  rm -rf /usr/local/libexec/umm
  echo "purged"
else
  echo "removed (config and credential kept; pass --purge to remove them)"
fi
