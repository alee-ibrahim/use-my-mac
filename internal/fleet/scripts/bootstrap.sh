#!/bin/sh
# umm fleet bootstrap (runs as root via `sudo -S`, one time per Mac).
#
# Installs the root-owned managed scripts and a narrowly-scoped sudoers
# drop-in that lets the admin user run ONLY those scripts without a password.
# The scripts are root-owned and live in a directory the admin cannot write,
# so NOPASSWD on them is not a root escalation.
#
#   bootstrap.sh <stage-dir> <admin-user>
set -eu

STAGE="${1:?usage: bootstrap.sh <stage-dir> <admin-user>}"
ADMIN="${2:?usage: bootstrap.sh <stage-dir> <admin-user>}"
LIBEXEC="/usr/local/libexec/umm"
SUDOERS="/etc/sudoers.d/umm"

[ "$(id -u)" -eq 0 ] || { echo "bootstrap must run as root" >&2; exit 1; }

# Root-owned script directory (not writable by the admin user).
install -d -m 0755 -o root -g wheel "$LIBEXEC"
for s in install upgrade uninstall; do
  [ -f "$STAGE/$s" ] || { echo "missing $STAGE/$s" >&2; exit 1; }
  install -m 0755 -o root -g wheel "$STAGE/$s" "$LIBEXEC/$s"
done

# Render and validate the sudoers drop-in before putting it in place.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
cat > "$tmp" <<EOF
# Managed by use-my-mac fleet. Do not edit by hand.
# Grants NOPASSWD only for the root-owned umm install scripts.
Cmnd_Alias UMM_CMDS = $LIBEXEC/install, $LIBEXEC/uninstall, $LIBEXEC/upgrade
Defaults!UMM_CMDS !requiretty
$ADMIN ALL=(root) NOPASSWD: UMM_CMDS
EOF
chmod 0440 "$tmp"
# visudo -cf refuses to install a syntactically invalid file.
visudo -cf "$tmp" >/dev/null
install -m 0440 -o root -g wheel "$tmp" "$SUDOERS"

echo "bootstrap ok: ${ADMIN} may now run ${LIBEXEC}/{install,upgrade,uninstall} without a password"
