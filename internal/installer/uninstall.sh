#!/bin/sh
# use-my-mac uninstaller:
#   curl -fsSL <router>/i/uninstall | sudo sh                 # remove the agent, keep config and credential
#   curl -fsSL <router>/i/uninstall | sudo sh -s -- --purge   # also remove config, credential, logs and _umm
# Finds the agent's LaunchDaemons by what they run, so any service label works.
# Also delete the Mac in the dashboard so its credential is revoked.
set -eu

main() {
R=''
die() { printf 'use-my-mac: error: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "run it with sudo"
[ "$(uname -s)" = Darwin ] || die "run it on a Mac"

found=0
for pass in updater agent; do
  for p in "$R"/Library/LaunchDaemons/*.plist; do
    [ -f "$p" ] || continue
    grep -q "<string>$R/usr/local/bin/umm-agent</string>" "$p" || continue
    label="$(basename "$p" .plist)"
    case "$pass:$label" in
      updater:*.updater|agent:*) ;;
      *) continue ;;
    esac
    launchctl bootout "system/$label" 2>/dev/null || true
    rm -f "$p"
    found=1
  done
done
rm -f "$R/usr/local/bin/umm-agent" "$R/usr/local/bin/.umm-agent.new" "$R/usr/local/bin/.umm-agent.install"
rm -rf "$R/usr/local/var/umm-updater"

if [ "${1:-}" = "--purge" ]; then
  rm -rf "$R/usr/local/etc/umm" "$R/usr/local/var/umm" "$R/usr/local/var/log/umm" "$R/usr/local/var/log/umm-updater.log"
  dscl . -delete /Users/_umm 2>/dev/null || true
  dscl . -delete /Groups/_umm 2>/dev/null || true
  printf 'use-my-mac: agent removed and purged. Delete the Mac in the dashboard too.\n'
elif [ "$found" = 1 ]; then
  printf 'use-my-mac: agent removed (config and credential kept; add "-s -- --purge" to sh to remove them). Delete the Mac in the dashboard too.\n'
else
  printf 'use-my-mac: no agent LaunchDaemons found; removed leftover files.\n'
fi
}

main "$@"
