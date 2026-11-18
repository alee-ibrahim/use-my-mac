#!/bin/sh
# umm fleet upgrade (root-owned; run via `sudo -n .../upgrade <stage>`).
# Replaces the agent binary and restarts the LaunchDaemon. Config, credential
# and the service user are left untouched.
set -eu

STAGE="${1:?usage: upgrade <stage-dir>}"
BIN="/usr/local/bin/umm-agent"
LABEL="$(cat "$STAGE/label" 2>/dev/null || echo io.github.alee-ibrahim.umm-agent)"
PLIST="/Library/LaunchDaemons/${LABEL}.plist"

[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }
[ -f "$STAGE/umm-agent" ] || { echo "missing $STAGE/umm-agent" >&2; exit 1; }

install -m 0755 -o root -g wheel "$STAGE/umm-agent" "$BIN"
xattr -d com.apple.quarantine "$BIN" 2>/dev/null || true

if ! launchctl kickstart -k "system/${LABEL}" 2>/dev/null; then
  launchctl bootout "system/${LABEL}" 2>/dev/null || true
  [ -f "$PLIST" ] && launchctl bootstrap system "$PLIST" || true
fi
echo "upgraded: $("$BIN" version 2>/dev/null || echo unknown)"
