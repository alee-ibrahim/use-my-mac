#!/bin/sh
# umm fleet install (root-owned; run via `sudo -n .../install <stage>`).
# Idempotent: safe to re-run. Installs/updates the agent, writes config only if
# absent, enrols with a one-time token read from a staged root-only file (never
# argv/env), and (re)loads the LaunchDaemon.
set -eu

STAGE="${1:?usage: install <stage-dir>}"
SVC_USER="_umm"
BIN="/usr/local/bin/umm-agent"
ETC="/usr/local/etc/umm"
VAR="/usr/local/var/umm"
LOG="/usr/local/var/log/umm"
LABEL="$(cat "$STAGE/label" 2>/dev/null || echo io.github.alee-ibrahim.umm-agent)"
PLIST="/Library/LaunchDaemons/${LABEL}.plist"

die() { echo "error: $*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "must run as root"
[ -f "$STAGE/umm-agent" ] || die "missing $STAGE/umm-agent"
[ -f "$STAGE/plist" ] || die "missing $STAGE/plist"

# --- hidden, non-admin service user (same as deploy/mac/install.sh) --------
if ! dscl . -read "/Users/${SVC_USER}" >/dev/null 2>&1; then
  uid=""
  for candidate in $(seq 450 499); do
    if ! dscl . -list /Users UniqueID | awk '{print $2}' | grep -qx "$candidate" &&
       ! dscl . -list /Groups PrimaryGroupID | awk '{print $2}' | grep -qx "$candidate"; then
      uid=$candidate; break
    fi
  done
  [ -n "$uid" ] || die "no free uid in 450-499"
  echo "creating service user ${SVC_USER} (uid ${uid})"
  dscl . -create "/Groups/${SVC_USER}"
  dscl . -create "/Groups/${SVC_USER}" PrimaryGroupID "$uid"
  dscl . -create "/Groups/${SVC_USER}" RealName "use-my-mac agent"
  dscl . -create "/Users/${SVC_USER}"
  dscl . -create "/Users/${SVC_USER}" UniqueID "$uid"
  dscl . -create "/Users/${SVC_USER}" PrimaryGroupID "$uid"
  dscl . -create "/Users/${SVC_USER}" UserShell /usr/bin/false
  dscl . -create "/Users/${SVC_USER}" NFSHomeDirectory "$VAR"
  dscl . -create "/Users/${SVC_USER}" RealName "use-my-mac agent"
  dscl . -create "/Users/${SVC_USER}" IsHidden 1
  dscl . -create "/Users/${SVC_USER}" Password "*"
fi

# --- binary ---------------------------------------------------------------
mkdir -p /usr/local/bin
install -m 0755 -o root -g wheel "$STAGE/umm-agent" "$BIN"
xattr -d com.apple.quarantine "$BIN" 2>/dev/null || true

# --- directories ----------------------------------------------------------
install -d -m 0750 -o root -g "$SVC_USER" "$ETC"
install -d -m 0700 -o "$SVC_USER" -g "$SVC_USER" "$VAR" "$VAR/work"
install -d -m 0750 -o "$SVC_USER" -g "$SVC_USER" "$LOG"
if [ ! -f "$ETC/credential" ]; then
  touch "$ETC/credential"
  chown "$SVC_USER":"$SVC_USER" "$ETC/credential"
  chmod 0600 "$ETC/credential"
fi

# --- config (only if absent, so a local edit is never clobbered) ----------
if [ ! -f "$ETC/agent.yaml" ]; then
  [ -f "$STAGE/agent.yaml" ] || die "no $ETC/agent.yaml and none staged"
  install -m 0640 -o root -g "$SVC_USER" "$STAGE/agent.yaml" "$ETC/agent.yaml"
  echo "wrote $ETC/agent.yaml"
fi

# --- launchd plist --------------------------------------------------------
install -m 0644 -o root -g wheel "$STAGE/plist" "$PLIST"

# --- enrol (one-time token from a root-only staged file, via stdin) -------
if [ ! -s "$ETC/credential" ]; then
  if [ -f "$STAGE/enrol-token" ]; then
    tr -d '\r\n' < "$STAGE/enrol-token" | sudo -u "$SVC_USER" "$BIN" enrol -config "$ETC/agent.yaml"
    rm -f "$STAGE/enrol-token"
  else
    echo "no enrolment token staged; agent installed but not enrolled" >&2
  fi
fi

# --- (re)load -------------------------------------------------------------
launchctl bootout "system/${LABEL}" 2>/dev/null || true
if [ -s "$ETC/credential" ]; then
  launchctl bootstrap system "$PLIST"
  echo "installed: ${LABEL} running"
else
  echo "installed: ${LABEL} NOT started (no credential)"
  exit 2
fi
