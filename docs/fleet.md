# Fleet install (`umm fleet`)

`umm fleet` installs and manages the `umm-agent` across many Macs over SSH from
an **admin workstation** (Windows, macOS or Linux). It is the alternative to
running `deploy/mac/install.sh` by hand on each machine.

## Add a Mac with one command

Without SSH: create an install link in the dashboard (**Macs → Add Mac → One
command**) or with `umm enrol-command`, and run the command it shows in
Terminal on the Mac:

```sh
curl -fsSL https://umm.example.com/i/umm_ins_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX | sudo sh
```

For a batch, make one link good for N Macs (`-uses N`, or switch off "Single
use"). The script verifies the signed release, installs the same files and
daemons as `umm fleet install` (it shares the `agent.yaml` checksum rule, so
fleet and the one-liner can manage the same Mac), enrols and waits for the
first heartbeat. Remove it with
`curl -fsSL https://umm.example.com/i/uninstall | sudo sh`. Details and the
security model: [Usage: Add a Mac with one command](usage.md#add-a-mac-with-one-command).

Where things run, and where secrets live:

- `umm fleet` runs on the admin's own computer. It is **not** installed on the
  Macs and does **not** run in the router.
- **Agents never receive SSH keys.** The SSH key authenticates the admin to the
  Macs and stays on the admin workstation (or in ssh-agent).
- Secrets are never passed on the command line (and so never show up in `ps`):
  the SSH key is referenced by path, the admin **sudo password** and the
  **router admin API key** come from a file, a named environment variable, or a
  no-echo prompt. The sudo password is fed to `sudo -S` over stdin; the
  enrolment token reaches the Mac as a root-only file and is handed to the agent
  over stdin.

## Inventory

A YAML file (default `./fleet.yaml`) lists the hosts, connection defaults and
the router URL. See [`deploy/mac/fleet.example.yaml`](../deploy/mac/fleet.example.yaml).

```yaml
router: https://umm.example.com
bundle: ./dist/umm-agent-darwin-arm64        # dir with the umm-agent binary
defaults:
  user: fleetadmin                           # an admin that can sudo on each Mac
  ssh_key: ~/.ssh/umm_fleet_ed25519
  runtimes: { ffmpeg: ["/opt/homebrew/bin/ffmpeg"] }
hosts:
  - host: mac-a.example.com
  - host: mac-b.example.com
    name: studio-b
    service_label: com.example.helper        # optional neutral label
```

Defaults are merged into each host; a host can override `port`, `user`,
`ssh_key`, `name`, `max_tasks`, `priority`, `labels`, `runtimes` and
`service_label`. Unknown keys are rejected so typos surface.

## Prerequisites on each Mac

- An **admin account** (the inventory `user`) reachable over SSH with the
  admin's public key in `~/.ssh/authorized_keys`, and able to run `sudo`.
- Remote Login (SSH) enabled (System Settings → General → Sharing).
- The agent bundle built on the admin workstation: `make agent-darwin`.

## Commands

```sh
# Build the agent bundle the inventory points at.
make agent-darwin

# 1) One-time per Mac: install the sudoers drop-in + root-owned scripts.
#    This is the only step that needs the admin's sudo password.
umm fleet bootstrap -inventory fleet.yaml -sudo-password-file ~/.umm/sudo.txt

# 2) Install the agent, enrol it and verify it reports telemetry.
#    Needs a router admin API key to mint the one-time enrolment token.
umm fleet install -inventory fleet.yaml -admin-key-file ~/.umm/admin.key

# Later:
umm fleet upgrade   -inventory fleet.yaml          # replace binary + restart
umm fleet status    -inventory fleet.yaml          # version / installed / telemetry
umm fleet uninstall -inventory fleet.yaml          # remove agent
umm fleet uninstall -inventory fleet.yaml -purge   # also remove config/user/sudoers
```

Re-running `install` applies inventory changes (runtimes, labels, ...) to
`agent.yaml` as long as the file on the Mac is still the one fleet wrote (fleet
keeps its checksum next to it). A file edited on the Mac is kept, with a
warning; pass `-replace-config` to overwrite it. After restarting the daemon,
`install` waits up to 45 s for a heartbeat from the new agent process before
reporting `telemetry: online`.

The managed scripts are root-owned, so a newer `umm` with changed scripts only
takes effect on a Mac after `umm fleet bootstrap` is run again there.

Add host names/IPs as positional arguments to act on a subset:
`umm fleet upgrade mac-a.example.com studio-b`.

## Agent self-update

`install` and `upgrade` also install the agent's **root updater**: a second
LaunchDaemon, `<service_label>.updater`, that runs `umm-agent update` at boot
and every 15 minutes. From then on the Macs follow the version the router asks
for (dashboard Settings, **Agent updates**), installing only signed releases
and rolling back on failure. See [Design](design.md#agent-self-update) and
[Usage](usage.md#agent-updates). After that, `umm fleet upgrade` is only
needed to repair a Mac or to change the LaunchDaemons themselves.

Macs installed before self-update existed have old managed scripts that do not
know about the updater. Give them the updater once:

```sh
umm fleet bootstrap -inventory fleet.yaml -sudo-password-file ~/.umm/sudo.txt   # new scripts (sudo password)
umm fleet upgrade   -inventory fleet.yaml                                        # installs the updater daemon
umm fleet status    -inventory fleet.yaml                                        # shows "updater"
```

The updater runs from launchd as root, so the sudoers drop-in keeps granting
only the three managed scripts. `uninstall` removes the updater too.

Runs are **parallel** (bounded by `-concurrency`, default 5), **idempotent**
(safe to re-run), and print a per-host result table. A non-zero exit means at
least one host failed.

### Secrets and keys

| What | Where it comes from |
| --- | --- |
| SSH private key | `ssh_key` in the inventory, `-ssh-key`, or ssh-agent (`SSH_AUTH_SOCK`). Encrypted keys must be loaded into ssh-agent. |
| sudo password (bootstrap only) | `-sudo-password-file PATH`, `-sudo-password-env NAME`, or a no-echo prompt. Fed to `sudo -S` over stdin. |
| router admin API key (install/status) | `-admin-key-file PATH`, `-admin-key-env NAME`, or the usual `UMM_API_KEY` / `~/.config/umm/api-key`. |

### Host key verification

Host keys are checked against `known_hosts` (default `~/.ssh/known_hosts`,
override with `-known-hosts`). For a first rollout, `-accept-new-host-keys`
trusts an unknown host on first use (TOFU) and records its key; a key that later
*conflicts* with the recorded one is always rejected. This uses
`golang.org/x/crypto/ssh`, so it behaves the same on Windows as on macOS/Linux
(no dependency on a system `ssh`/`scp`).

## How bootstrap stays safe

`bootstrap` is what lets later steps run without a password. It must not become a
root escalation, so:

- The three managed scripts are installed **root-owned, mode 0755** under
  `/usr/local/libexec/umm/` (a directory the admin user cannot write). If the
  admin could modify a script, NOPASSWD on it would be equivalent to full root.
- `/etc/sudoers.d/umm` grants the admin `NOPASSWD` for **only** those three
  script paths (via a `Cmnd_Alias`), nothing else:

  ```
  Cmnd_Alias UMM_CMDS = /usr/local/libexec/umm/install, /usr/local/libexec/umm/uninstall, /usr/local/libexec/umm/upgrade
  Defaults!UMM_CMDS !requiretty
  fleetadmin ALL=(root) NOPASSWD: UMM_CMDS
  ```

- The drop-in is validated with `visudo -cf` **before** it is moved into place,
  so a bad file can never break `sudo` on the Mac.

After bootstrap, `install`, `upgrade` and `uninstall` run via `sudo -n`
(non-interactive, no password).

## Enrolment

`install` mints a **one-time enrolment token** per Mac through the router admin
API, stages it as a `0600` root-only file, and the install script pipes it to
`umm-agent enrol` over **stdin**. The token never appears in `argv` or the
environment (so it is never visible in `ps`). The agent exchanges it for a
per-Mac credential stored `0600` under `_umm`. Re-running `install` on an
already-enrolled Mac mints a fresh token that simply expires unused.

## Quiet install

The agent is already unobtrusive: it runs as the hidden, non-admin `_umm`
service user, has no UI and shows no notifications of its own. For an install
staff are not meant to notice:

- Set a neutral `service_label` (e.g. `com.example.helper`) in the inventory.
  `umm fleet` renders the LaunchDaemon plist and all scripts around that label.

- **macOS will still notify the logged-in user.** On macOS 13+, the first time a
  new `LaunchDaemon` appears the OS shows a **"Background Items Added"**
  notification and lists the item under Login Items & Extensions → Background
  Task Management. Nothing the daemon does from its own side suppresses this.

- The only reliable suppression is an **MDM-delivered managed-login-items
  profile** (`com.apple.servicemanagement`) that marks the item as managed. An
  example is in
  [`deploy/mac/managed-login-items.mobileconfig`](../deploy/mac/managed-login-items.mobileconfig)
  — edit the `TeamIdentifier` and `LabelPrefix`, then push it with your MDM
  (it has no effect if installed by hand on a supervised Mac).

## What runs where (summary)

| Step | Privilege | Password? | Script |
| --- | --- | --- | --- |
| `bootstrap` | `sudo -S` (root) | yes, once | installs `/usr/local/libexec/umm/{install,upgrade,uninstall}` + `/etc/sudoers.d/umm` |
| `install` | `sudo -n` (root) | no | creates `_umm`, installs binary/config/plists, enrols, loads the agent and updater daemons |
| `upgrade` | `sudo -n` (root) | no | replaces the binary, restarts the daemon, installs or refreshes the updater daemon |
| `uninstall` | `sudo -n` (root) | no | stops and removes the agent (`-purge` also removes state, user, sudoers) |
| `status` | none | no | reads agent version / daemon and updater presence + router telemetry |
