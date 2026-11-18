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
  # Not loaded (or kickstart refused): load it. Same bootout race as install.
  launchctl bootout "system/${LABEL}" 2>/dev/null || true
  i=0
  while launchctl print "system/${LABEL}" >/dev/null 2>&1 && [ "$i" -lt 100 ]; do
    i=$((i + 1)); sleep 0.2
  done
  if [ -f "$PLIST" ]; then
    n=0
    until launchctl bootstrap system "$PLIST"; do
      n=$((n + 1)); [ "$n" -lt 5 ] || { echo "launchctl bootstrap ${LABEL} failed" >&2; exit 1; }
      sleep 1
    done
  fi
fi
echo "upgraded: $("$BIN" version 2>/dev/null || echo unknown)"
