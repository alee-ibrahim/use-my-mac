#!/bin/sh
# Install the use-my-mac agent on an Apple Silicon Mac.
#
#   sudo UMM_SERVER=https://umm.example.com sh install.sh
#
# What it does (idempotent):
#   * creates a hidden, non-admin service user "_umm" (no login shell)
#   * installs umm-agent to /usr/local/bin (from ./umm-agent next to this
#     script, or from $UMM_AGENT_BINARY)
#   * writes /usr/local/etc/umm/agent.yaml (only if it does not exist)
#   * installs a LaunchDaemon that runs the agent as _umm
#   * installs the root updater LaunchDaemon (signed self-updates; see
#     docs/design.md "Agent self-update")
#   * enrols the Mac with a one-time token (prompted, or UMM_ENROL_TOKEN)
#   * creates the hidden work volume and, if configured, the task network
#     policy (`umm-agent prepare`; see docs/design.md "Task isolation")
#
# Secrets are never passed as arguments: the enrolment token is read from the
# environment or typed at the prompt, and the credential is stored in a 0600
# file owned by _umm.
set -eu

LABEL="io.github.alee-ibrahim.umm-agent"
SVC_USER="_umm"
BIN="/usr/local/bin/umm-agent"
ETC="/usr/local/etc/umm"
VAR="/usr/local/var/umm"
LOG="/usr/local/var/log/umm"
PLIST="/Library/LaunchDaemons/${LABEL}.plist"
UPD_PLIST="/Library/LaunchDaemons/${LABEL}.updater.plist"
UPD_STATE="/usr/local/var/umm-updater"
HERE="$(cd "$(dirname "$0")" && pwd)"

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

[ "$(id -u)" -eq 0 ] || die "run with sudo"
[ "$(uname -s)" = "Darwin" ] || die "macOS only"
[ "$(uname -m)" = "arm64" ] || echo "warning: not Apple Silicon; GPU/VideoToolbox telemetry may be missing" >&2

# --- service user --------------------------------------------------------
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

# --- binary --------------------------------------------------------------
SRC="${UMM_AGENT_BINARY:-$HERE/umm-agent}"
[ -f "$SRC" ] || die "agent binary not found at $SRC (build it with 'make agent-darwin' or set UMM_AGENT_BINARY)"
parent_dirs /usr/local /usr/local/bin /usr/local/etc /usr/local/var /usr/local/var/log
install -m 0755 -o root -g wheel "$SRC" "$BIN"
xattr -d com.apple.quarantine "$BIN" 2>/dev/null || true

# --- directories ---------------------------------------------------------
install -d -m 0750 -o root -g "$SVC_USER" "$ETC"
install -d -m 0700 -o "$SVC_USER" -g "$SVC_USER" "$VAR" "$VAR/work"
install -d -m 0750 -o "$SVC_USER" -g "$SVC_USER" "$LOG"
if [ ! -f "$ETC/credential" ]; then
  touch "$ETC/credential"
  chown "$SVC_USER":"$SVC_USER" "$ETC/credential"
  chmod 0600 "$ETC/credential"
fi

# --- config --------------------------------------------------------------
if [ ! -f "$ETC/agent.yaml" ]; then
  [ -n "${UMM_SERVER:-}" ] || die "set UMM_SERVER=https://your-router for the first install"
  name="$(scutil --get ComputerName 2>/dev/null || hostname -s)"
  {
    echo "server: ${UMM_SERVER}"
    echo "name: \"${name}\""
    echo "credential_file: ${ETC}/credential"
    echo "work_dir: ${VAR}/work"
    echo "max_tasks: 1"
    echo "priority: adaptive"
    echo "pause_file: ${ETC}/paused"
    echo "user_load_scope: console"
    echo "# Only these programs can be run. Tasks pick one by name and add arguments."
    echo "runtimes:"
    found=0
    for tool in /opt/homebrew/bin/ffmpeg /usr/local/bin/ffmpeg; do
      if [ -x "$tool" ]; then echo "  ffmpeg: [\"$tool\"]"; found=1; break; fi
    done
    [ "$found" -eq 1 ] || echo "  # ffmpeg: [\"/opt/homebrew/bin/ffmpeg\"]"
    echo "  # ocr: [\"/usr/bin/python3\", \"/opt/ocr/run.py\"]"
    echo "yield:"
    echo "  user_cpu_high: 50      # user's own CPU % (ours excluded) that counts as heavy"
    echo "  user_cpu_low: 25"
    echo "  sustain_seconds: 10    # heavy this long -> stop our tasks"
    echo "  cooldown_seconds: 120  # calm this long -> accept work again"
    echo "  memory_pressure: warn"
    echo "  thermal: serious"
    echo "  yield_on_battery: true"
  } > "$ETC/agent.yaml"
  chown root:"$SVC_USER" "$ETC/agent.yaml"
  chmod 0640 "$ETC/agent.yaml"
  echo "wrote $ETC/agent.yaml — review the runtimes allowlist"
fi

# --- launchd -------------------------------------------------------------
PLIST_SRC="$HERE/${LABEL}.plist"
[ -f "$PLIST_SRC" ] || die "missing $PLIST_SRC"
install -m 0644 -o root -g wheel "$PLIST_SRC" "$PLIST"
UPD_SRC="$HERE/${LABEL}.updater.plist"
if [ -f "$UPD_SRC" ]; then
  install -d -m 0700 -o root -g wheel "$UPD_STATE"
  install -m 0644 -o root -g wheel "$UPD_SRC" "$UPD_PLIST"
fi

# --- enrol ---------------------------------------------------------------
if [ ! -s "$ETC/credential" ]; then
  if [ -z "${UMM_ENROL_TOKEN:-}" ] && [ -t 0 ]; then
    printf "One-time enrolment token (dashboard -> Macs -> Add a Mac): "
    stty -echo; read -r UMM_ENROL_TOKEN; stty echo; echo
    export UMM_ENROL_TOKEN
  fi
  if [ -n "${UMM_ENROL_TOKEN:-}" ]; then
    sudo -u "$SVC_USER" --preserve-env=UMM_ENROL_TOKEN "$BIN" enrol -config "$ETC/agent.yaml"
    unset UMM_ENROL_TOKEN
  else
    echo
    echo "Next: enrol this Mac with a one-time token from the dashboard (Macs -> Add a Mac):"
    echo "  sudo -u ${SVC_USER} ${BIN} enrol -config ${ETC}/agent.yaml"
    echo "then start it:"
    echo "  sudo launchctl bootstrap system ${PLIST}"
    exit 0
  fi
fi

"$BIN" prepare -config "$ETC/agent.yaml" -label "$LABEL" || echo "warning: host preparation failed; the agent reports why" >&2

launchctl bootout system "$PLIST" 2>/dev/null || true
launchctl bootstrap system "$PLIST"
if [ -f "$UPD_PLIST" ]; then
  launchctl bootout system "$UPD_PLIST" 2>/dev/null || true
  launchctl bootstrap system "$UPD_PLIST" || echo "warning: could not load the updater daemon" >&2
fi
echo "umm-agent installed and running (logs: $LOG/agent.log, updater: /usr/local/var/log/umm-updater.log)"
echo "Pause work on this Mac at any time:  sudo touch $ETC/paused   (remove the file to resume)"
