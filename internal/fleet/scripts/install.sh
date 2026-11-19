#!/bin/sh
# umm fleet install (root-owned; run via `sudo -n .../install <stage>`).
# Idempotent: safe to re-run. Installs/updates the agent, writes or updates the
# config (never over a local edit), enrols with a one-time token read from a
# staged root-only file (never argv/env), and (re)loads the LaunchDaemon.
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

# _umm must be able to traverse every parent of its own directories. A missing
# parent is created 0755 root:wheel (umask 022, so intermediate directories do
# not inherit a restrictive umask from sudo). An existing one only gets o+x:
# its owner and other bits stay as they are (Homebrew may own /usr/local on
# Intel Macs).
parent_dirs() {
  for d do
    if [ -d "$d" ]; then
      chmod o+x "$d"
    else
      (umask 022; install -d -m 0755 -o root -g wheel "$d")
    fi
  done
}
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
parent_dirs /usr/local /usr/local/bin /usr/local/etc /usr/local/var /usr/local/var/log
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

# --- config ----------------------------------------------------------------
# fleet records the checksum of every agent.yaml it writes. A re-run replaces
# the config when the inventory changed AND the file on the Mac is still the
# one fleet wrote; a local edit is never clobbered unless -replace-config.
SUM="$ETC/agent.yaml.fleet-sha256"
sha() { shasum -a 256 "$1" | awk '{print $1}'; }
write_config() {
  install -m 0640 -o root -g "$SVC_USER" "$STAGE/agent.yaml" "$ETC/agent.yaml"
  sha "$ETC/agent.yaml" > "$SUM"
  chmod 0644 "$SUM"
}
if [ ! -f "$ETC/agent.yaml" ]; then
  [ -f "$STAGE/agent.yaml" ] || die "no $ETC/agent.yaml and none staged"
  write_config
  echo "wrote $ETC/agent.yaml"
elif [ -f "$STAGE/agent.yaml" ]; then
  cur="$(sha "$ETC/agent.yaml")"
  if [ "$cur" = "$(sha "$STAGE/agent.yaml")" ]; then
    [ -f "$SUM" ] || sha "$ETC/agent.yaml" > "$SUM"   # adopt an identical file
  elif [ -f "$STAGE/replace-config" ] || { [ -f "$SUM" ] && [ "$(cat "$SUM")" = "$cur" ]; }; then
    write_config
    echo "updated $ETC/agent.yaml"
  else
    echo "warning: kept $ETC/agent.yaml (edited on this Mac, or written before fleet tracked it); re-run install with -replace-config to apply the inventory" >&2
  fi
fi

# --- launchd plist --------------------------------------------------------
install -m 0644 -o root -g wheel "$STAGE/plist" "$PLIST"

# --- updater: root LaunchDaemon for signed self-updates --------------------
UPD_LABEL="${LABEL}.updater"
UPD_PLIST="/Library/LaunchDaemons/${UPD_LABEL}.plist"
if [ -f "$STAGE/updater-plist" ]; then
  install -d -m 0700 -o root -g wheel /usr/local/var/umm-updater
  install -m 0644 -o root -g wheel "$STAGE/updater-plist" "$UPD_PLIST"
fi

# --- enrol (one-time token from a root-only staged file, via stdin) -------
if [ ! -s "$ETC/credential" ]; then
  if [ -f "$STAGE/enrol-token" ]; then
    tr -d '\r\n' < "$STAGE/enrol-token" | sudo -u "$SVC_USER" "$BIN" enrol -config "$ETC/agent.yaml"
    rm -f "$STAGE/enrol-token"
  else
    echo "no enrolment token staged; agent installed but not enrolled" >&2
  fi
fi

# --- work volume and task network policy (root; not fatal) ----------------
"$BIN" prepare -config "$ETC/agent.yaml" -label "$LABEL" || echo "warning: host preparation failed; the agent reports why" >&2

# --- (re)load -------------------------------------------------------------
# Re-running against a loaded daemon: bootout can return before launchd has
# torn the job down, and Background Task Management may briefly reject the
# re-registration ("invalid record generation"). Either makes bootstrap fail
# with "5: Input/output error", leaving the agent stopped. Wait for the old job
# to go, then retry bootstrap a few times.
launchctl bootout "system/${LABEL}" 2>/dev/null || true
i=0
while launchctl print "system/${LABEL}" >/dev/null 2>&1; do
  i=$((i + 1)); [ "$i" -le 100 ] || die "previous ${LABEL} did not stop"
  sleep 0.2
done
if [ -s "$ETC/credential" ]; then
  n=0
  until launchctl bootstrap system "$PLIST"; do
    n=$((n + 1)); [ "$n" -lt 5 ] || die "launchctl bootstrap ${LABEL} failed"
    sleep 1
  done
  if [ -f "$UPD_PLIST" ]; then
    launchctl bootout "system/${UPD_LABEL}" 2>/dev/null || true
    i=0
    while launchctl print "system/${UPD_LABEL}" >/dev/null 2>&1 && [ "$i" -lt 100 ]; do
      i=$((i + 1)); sleep 0.2
    done
    n=0
    until launchctl bootstrap system "$UPD_PLIST"; do
      n=$((n + 1)); [ "$n" -lt 5 ] || { echo "warning: could not load ${UPD_LABEL}" >&2; break; }
      sleep 1
    done
  fi
  echo "installed: ${LABEL} running"
else
  echo "installed: ${LABEL} NOT started (no credential)"
  exit 2
fi
