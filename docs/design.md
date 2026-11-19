# Design

This document covers how use-my-mac decides where work runs, how it stays out of the user's way, where
files go, how it is secured and what happens when things fail.

## Components

| Component | Runs on | Role |
| --- | --- | --- |
| `umm-server` | Kubernetes or any Linux/macOS host | REST API, scheduler, agent endpoint, storage relay, dashboard, metrics |
| `umm-agent` | each Mac (LaunchDaemon, user `_umm`) | telemetry, yield policy, runs tasks, streams logs, moves files |
| `umm` | anywhere | CLI for apps and admins |
| `pkg/client` | your app | Go SDK (thin REST wrapper) |
| dashboard | browser | React + official shadcn/ui components, served by `umm-server` |

Code layout: `internal/sched` (pure scoring, no I/O), `internal/engine` (state machine on top of the
store), `internal/store` (SQL), `internal/storage` (blob backends), `internal/agent` (agent, yield policy,
argv/env building), `internal/agent/telemetry` (collectors and parsers), `internal/server` (HTTP),
`internal/auth` (secrets, sessions, OIDC).

## Data model

- **Job:** a name, labels, an owner (the API key or OIDC subject that submitted it) and a set of tasks.
  Job state is derived: `queued`, `running`, then `succeeded`, `failed` or `cancelled`.
- **Task:** `runtime` (an allowlisted name), `args`, `env`, `inputs`, `outputs`, `resources`
  (`ram_mb`, `gpu`, `expected_seconds`, `disk_mb`, `labels`), `timeout_seconds`, `max_retries` and
  `depends_on`. States: `blocked`, `pending`, `assigned`, `running`, then `succeeded`, `failed` or
  `cancelled`.
- **Attempt:** one try of a task on one Mac, with its placement score and outcome: `succeeded`, `failed`,
  `timeout`, `yielded`, `lost` or `cancelled`.

Jobs are small DAGs. An input `from: {task, output}` adds an implicit dependency. Cycles, unknown references
and inputs with zero or several sources are rejected at submit time. A dependency that fails or is cancelled
cancels its dependents, and the job ends `failed`.

## Scheduling

Every tick (1 s by default) and on every relevant event (submit, finish, a Mac becoming ready) the
scheduler:

1. **Requeues lost work.** That covers tasks on Macs whose heartbeat is older than `heartbeat_timeout`,
   assignments not picked up within `assign_timeout`, and assignments to a Mac that stopped being `ready`.
   None of these count as failures.
2. **Fails overdue attempts** that are past `deadline + timeout_grace` without a report from the agent.
3. **Unblocks** tasks whose dependencies all succeeded.
4. **Places** runnable tasks, oldest first. Each Mac is scored with `sched.Score`. The best Mac gets the task,
   and the next placement in the same tick sees the updated load. That is how work spreads.

### Hard filters

A Mac is skipped (with a reason shown in the dashboard) if it is offline, drained, opted out, quarantined,
or not `ready` (yielding, cooldown, paused). It is also skipped if it is excluded for this task, lacks the
runtime (or has it allowlisted but unavailable: the agent's preflight failed, or a task could not execute it), has non-matching labels or is at `max_tasks`. Resource reasons: the task needs more RAM than the Mac
has, or free memory minus RAM reserved by our other tasks on it is too small. Machine-state reasons: critical
memory pressure, critical thermal state, too little free disk, or running on battery (unless
`allow_battery`, and then only above `min_battery_percent`).

### Score

The score is a weighted mean of factors in `[0, 1]`, scaled to 0–100. Weights are editable at runtime in
Settings.

| Factor | Value | Default weight |
| --- | --- | --- |
| presence | nobody logged in 1.0 · screen locked 0.95 · idle ≥ `idle_full_seconds` 0.85 · otherwise 0.2 → 0.8 with idle time | 3 (× up to 2 for long tasks) |
| load | `1 − (user CPU + our CPU)` | 2 |
| spread | `1 − active / max_tasks` | 1.5 |
| memory | free memory after placement / total, ×2 (×0.4 under `warn` pressure) | 1 |
| power | AC 1.0 · battery 0.3 | 1 |
| thermal | nominal 1.0 · fair 0.6 · serious 0.15 | 1 |
| gpu | `1 − GPU utilisation` (only for `gpu: true` tasks; unknown = 0.5) | 1.5 |
| capacity | CPU cores / largest Mac's cores | 0.5 |

Ties break on Mac ID, so placement is deterministic. Every decision is kept, with a per-factor breakdown,
and shown under a task's **Placement** tab.

## "Don't disturb the user": the yield policy

The agent runs a small state machine (`internal/agent/yield.go`) on every heartbeat:

```text
ready ──(user heavy for sustain_seconds, or a critical condition)──▶ yielding
yielding ──(user calm)──▶ cooldown
cooldown ──(heavy again)──▶ yielding
cooldown ──(calm for cooldown_seconds)──▶ ready
```

- **Heavy** means one of: the user's own CPU ≥ `user_cpu_high`, memory pressure ≥ the configured level,
  thermal state ≥ the configured level, or on battery when `yield_on_battery` is set.
- **Immediate**, with no sustain period: critical memory pressure, critical thermal state, battery.
- **Calm** means not heavy and user CPU < `user_cpu_low`. The gap between high and low gives hysteresis.
- **Our load never counts.** Processes in our task process groups, and processes of the `_umm` user, are
  accounted as "ours" and excluded from user load. A user who is just logged in, even an active one, never
  triggers a yield. This is the point of the design: staff Macs keep contributing until their owner
  actually needs the power.
- On entering `yielding` the agent sends SIGTERM to every running task's process group, then SIGKILL
  after `stop_grace`. It reports `yielded`, and the router immediately requeues the task without counting
  a failure. The task is excluded from that Mac for `yield_exclude` (5 min by default).
- While not `ready` the agent stops polling for work, and the router withdraws any assignment the agent
  had not yet picked up.
- **Adaptive priority** (`priority: adaptive`, the default): when nobody is using the Mac (no console user,
  screen locked, or no keyboard/mouse input for `priority_idle`, 5 min by default) tasks run at `utility`
  QoS (`taskpolicy -c utility` + `nice`): all cores, still below the user's apps. While someone is using
  it they run at `background` QoS, which on Apple Silicon keeps CPU work on the efficiency cores. A running
  task moves between the two in place: when the user comes back the agent sets darwin background state on
  every process in the task's group (`setpriority(PRIO_DARWIN_PROCESS)`, what `taskpolicy -b -p` does),
  and lifts it when they leave. It re-applies it each heartbeat while the user is there so new children
  are covered. A priority change never stops a task; the yield rules above are unchanged. Fixed
  `background`, `utility` and `none` are still available.
- **Local opt-out:** while `pause_file` exists the Mac is `draining` and stops its tasks. Admins can
  also **drain** a Mac from the dashboard (running tasks finish, no new work) or **opt it out** (running
  tasks are stopped and moved now).

## Storage

The router never streams task data through the agent connection. Agents receive short-lived URLs:

| Backend | Agent gets | Router holds |
| --- | --- | --- |
| `local` (default) | HMAC-signed `GET`/`PUT` URLs served by the router at `/v1/agent/blob`, bound to method, key and expiry | files on its disk/PVC |
| `s3` | SigV4 presigned URLs (any S3-compatible store), max 7 days | access key (from env) |
| plain URLs | the URL the app put in `inputs[].url` / `outputs[].url` | nothing |

URL lifetime is the task timeout plus 30 minutes. Keys are validated (no absolute paths, `..` or odd
characters). Outputs land at `jobs/<job>/<task-key>/<output>`, and app uploads at `uploads/<key>`. Reduce
inputs `from: {task, output}` resolve to the producing task's output URL at dispatch time.

**Multipart uploads.** Both backends implement `storage.Multipart`: S3 with its native multipart API
(part URLs are presigned `UploadPart` requests), `local` by keeping parts as files under `_multipart/<id>/`
and concatenating them on completion (part URLs are HMAC-signed `PUT`s, accepted only while that upload
exists). The `uploads` table maps our upload ID to the backend's, with owner, size, part size and expiry;
the bytes never touch the database. Apps send parts through the router (relay) or, with S3 and `direct`,
straight to the bucket. Agents always get presigned or signed part URLs, so they never send bytes with their
bearer credential and still hold no storage credentials. Stored parts are listed from the backend and only
counted when their size is exactly right, so a retried or resumed part can never assemble a wrong file.
Completion deletes the row; the scheduler loop aborts uploads older than 24 hours every 10 minutes
(`AbortMultipart` deletes the parts).

**Chunked downloads.** The router serves local blobs with `http.ServeContent`, so `Range` works. Agents and
the CLI download in parallel ranges and fall back to one stream when the server ignores `Range`.

## Live progress

Agents send `POST /v1/agent/tasks/{id}/progress` with the attempt, phase (`downloading`, `running`,
`uploading`), a percent within the phase and transfer bytes and rate, at most once a second and only when
something changed (plus a keep-alive every 15 s). While the process runs, the percent comes from
`UMM_PROGRESS=N` lines (a tap on stdout/stderr that also splits on carriage returns, as ffmpeg uses), the
`$UMM_PROGRESS_FILE` file (read every 0.5 s) or a built-in output parser (ffmpeg). Reports for an attempt
that is not current, or from another agent, get `409`. Agents stop reporting if the router answers `404`
(an older router).

The router keeps the latest report per running task **in memory only**: losing it on a restart costs a
second. It is dropped whenever the attempt ends. Task views fold it into `phase` and an overall `percent`
(bands: downloading 0-5, running 5-95, uploading 95-100); job views count finished tasks as 100 and cap the
total at 99.9 until the job is terminal, so 100 % always means done.

Every job change (submission, state transitions, progress, deletion) is published to in-process
subscribers. `GET /v1/jobs/{id}/stream` and `GET /v1/jobs/stream` turn that into server-sent events:
changes are debounced (250 ms), reloaded and sent only when the JSON changed, with a periodic resync because
notifications are dropped rather than ever blocking the engine, and a keep-alive comment every 15 s. Clients
(CLI, SDK, dashboard) fall back to polling when a stream cannot be held open.

## Security model

- **No inbound ports on Macs.** Agents only make outbound HTTPS requests (heartbeat, long-poll, status,
  logs, blob URLs).
- **Enrolment:** an admin mints a one-time enrolment token (24 h by default, stored hashed, consumed
  atomically). The agent exchanges it for a per-Mac credential (`umm_agt_…`, 256-bit). Only its SHA-256 is
  stored server-side. On the Mac it lives in a 0600 file owned by `_umm`. Deleting the Mac in the dashboard
  revokes it.
- **One-line install** (`curl -fsSL <router>/i/<token> | sudo sh`): install tokens (`umm_ins_…`) are
  enrolment secrets bound to an install profile, stored hashed, short-lived (1 h by default, 7 days at most),
  good for 1 to N enrolments and revocable. The script is rendered per request; every embedded value is
  checked against a strict pattern and single-quoted, or yaml-encoded inside a quoted heredoc with a random
  delimiter. It verifies the release with the pinned minisign key (pure JavaScript under `osascript`, as
  stock macOS has no Ed25519/BLAKE2b tool) and the SHA-256 before installing anything. The token reaches the
  agent over stdin and `curl` through a config on stdin, never argv. Served only for an https `public_url`,
  rate-limited per client address and audited (fetch, download, enrolment, refusal, revocation). Trust: the
  script comes from the router, so this protects against a tampered cache or release host, not against a
  compromised router (as with any `curl | sh`).
- **API keys** (`umm_key_…`) are hashed at rest and have a role: `app` (submit, and see only its own
  jobs), `viewer` (read everything) or `admin`.
- **Dashboard:** OIDC authorization-code flow with PKCE and nonce. Roles come from a groups claim and/or
  an app-roles claim, matched against two rule sets (`internal/access`). **Config rules**
  (`admin_groups`, `viewer_groups`, `default_role`) are authoritative: read-only at runtime and a floor no
  other rule can lower, so the configured admins can never be locked out. **Dashboard rules** (database,
  managed by admins in Settings > Access, `/v1/access/*` or `umm access`) add on top: group ID or user
  (subject, object ID, email or UPN) to `admin`/`viewer`, plus a default role (`deny`/`viewer`; the
  effective default is the higher of config and dashboard). Precedence: user rule > group rules (highest
  wins) > default; then the config floor. Every sign-in (allowed or not) records the user's identifiers
  and groups, so admins can see who tried and grant access. The role is **re-evaluated on every request**
  from that record and the current rules (cached for at most 10 s, dropped on every local change); a
  session from before tracking, or one whose user is now denied, gets `401` and signs in again. Changes
  that would remove the acting admin's own role with no other signed-in admin left are refused (`409`).
  Every change is audited (who, when, what). Limit who can sign in at all with the provider's "assignment
  required". Microsoft Entra ID **groups overage** (no groups in the token) is detected and
  refused with a clear error unless a user rule matches. Use app roles, or "groups assigned to the
  application". Sessions are
  HMAC-signed, HttpOnly, SameSite=Lax cookies (Secure behind HTTPS). Cookie-authenticated writes require an
  `X-UMM-CSRF` header. With OIDC configured, a signed-out browser on any dashboard route goes straight to
  the provider (optional `domain_hint`/`prompt`) and returns to the page it asked for; return targets are
  same-origin paths only. A short guard cookie stops redirect loops, a failed callback lands on an error
  page with "Try again", and sign-out lands on a signed-out page. `/docs` stays public.
- **Dashboard API-key sign-in** (`auth.dashboard_api_key_login`): `auto` (default) allows it only when OIDC
  is not configured; `true` keeps it alongside OIDC as break-glass (at `/login`); `false` never. Admin and
  viewer keys only. Turning it off also invalidates existing API-key browser sessions. Bearer API keys for
  the REST API, the CLI and apps are not affected. `GET /auth/config` reports the effective setting.
- **Secrets are never in argv or config files.** The server reads `UMM_SECRET_KEY`,
  `UMM_DATABASE_URL`, `UMM_OIDC_CLIENT_SECRET`, `UMM_S3_*` and `UMM_BOOTSTRAP_ADMIN_KEY` from the
  environment. The agent reads its credential from a file or `UMM_AGENT_CREDENTIAL`, and the enrolment
  token from `UMM_ENROL_TOKEN` or stdin. The CLI reads its key from `UMM_API_KEY`, a key file or
  `~/.config/umm/api-key`.
- **What a task can do:** run one of the Mac's allowlisted runtimes (a fixed argv prefix with an absolute
  path) plus arguments. It cannot choose the executable. Allowlist specific scripts rather than bare
  interpreters. The environment is minimal: tasks cannot set `PATH`, `HOME`, `TMPDIR`, `UMM_*`, `DYLD_*` or
  `LD_*`. Tasks run as the non-admin `_umm` user in a fresh `0700` work dir that is deleted afterwards.
  Optionally `sandbox-exec` denies writes outside the work dir. Logs are capped per attempt.
- **Server hardening:** distroless non-root image, read-only root filesystem, request size limits,
  `nosniff`, `X-Frame-Options: DENY` for the UI.
- **Agent updates** are installed by a separate root updater and only if signed (next section).

## Agent self-update

The agent runs as the unprivileged `_umm` user and `/usr/local/bin/umm-agent` is root-owned, so the agent
cannot replace itself, and should not be able to. Updates are done by a **root updater**: the same binary
run as `umm-agent update` by its own LaunchDaemon (`<agent label>.updater`, no `UserName`, `RunAtLoad` plus
`StartInterval` 900 s, up to 2 min of random jitter per run). It needs no sudo, so the fleet's sudoers
drop-in is unchanged. One cycle:

```text
check ──(nothing to do / paused / refused)──▶ exit
  │ install vX
download SHA256SUMS + .minisig + bundle ──(bad signature / checksum / wrong version)──▶ report failed, exit
  │
drain: report "draining" (router drains the Mac) until no task runs or drain_timeout passes
  │
save previous binary, write state "pending", atomic swap, launchctl kickstart -k the agent
  │
wait ≤ verify_timeout (2 min) for a heartbeat with version vX received after the swap
  ├─ yes ─▶ report succeeded
  └─ no ──▶ restore previous binary, restart, report rolled_back, back off vX (1 h, 2 h, … 24 h)
```

**Policy (router).** The desired version comes from the router: `GET /v1/agent/update` (agent credential)
returns the target, whether a downgrade is allowed, the drain timeout and the router's live view of this
agent (version, running tasks, last heartbeat in router time). Admins set `paused`, `target_version`
(default: the router's own version), `canary_version` with `canary_percent` (a stable hash of the Mac ID)
and `canary_macs`, and `drain_timeout_seconds`, in Settings or `PUT /v1/updates`. A router built from a
non-release version (`dev`) proposes nothing unless a target is pinned. Agents already report their version
in every heartbeat; the updater reports each state change to `POST /v1/agent/update/report`. The router
keeps the latest state per Mac and the last 50 changes (dashboard: Macs, Version column and history).

**Downgrades** are refused unless an admin pinned that version (target or canary). A git-describe build
(`v0.1.1-3-gabc1234`) sorts above its tag, and a non-release build (`dev`) is only replaced by a pinned
version.

**Signing.** Releases are signed with **minisign** (Ed25519, BLAKE2b-prehashed). The release workflow
signs `SHA256SUMS` with the secret key from the `UMM_RELEASE_SIGNING_KEY` Actions secret
(`tools/relsign`; it refuses to run without a key, so an unsigned release cannot be published) and the
trusted comment `use-my-mac release <version> file:SHA256SUMS`. The public key is
`internal/selfupdate/release-signing.pub`, compiled into the agent (`agent.yaml` `update.public_key` can
override it for forks; the file is root-owned). Verification is pure Go, before anything is installed:

1. the `.minisig` exists and verifies with the pinned key (signature and trusted-comment signature);
2. the trusted comment names exactly the version being installed, so an older signed `SHA256SUMS` cannot be
   replayed as a newer release;
3. the bundle's SHA-256 matches its line in `SHA256SUMS`;
4. `umm-agent` is extracted from the bundle into a root-only staging dir and must report that version.

Anyone can check a release by hand with `minisign -Vm SHA256SUMS -P <key from release-signing.pub>`.

**Downloads.** With the relay on (default) the router fetches the three artifacts from the release host
once, caches them in its blob storage under `agent-releases/<version>/` and serves them to agents at
`/v1/agent/update/artifacts/<version>/<file>` (agent credential, allowlisted names only). Without internet on
the router, set `updates.offline` and upload the files with `PUT /v1/updates/artifacts/...`; with
`updates.relay: false` agents download from `update.release_url` (GitHub by default). In `auto` mode the
updater falls back to the release host when the relay fails. The relay is not trusted: a router or cache that
serves a different file fails verification.

**Draining.** A `draining` or `applying` report drains the Mac for 3 minutes, refreshed while the updater
waits, so a dead updater cannot leave a Mac drained. The agent sees `drain_reason: updating`, finishes its
tasks and takes no new work; after `drain_timeout_seconds` (1 h by default) the agent is restarted anyway and
its tasks are requeued as `lost` (no failure counted).

**On the Mac.** `/usr/local/var/umm-updater/` (root, 0700) holds `staging/`, `previous/umm-agent` (one
previous version) and `state.json`. The swap writes `/usr/local/bin/.umm-agent.new` and renames it over the
live binary, so the running agent keeps its file and launchd starts the new one. `state.json` records a
pending update before the swap: if the updater dies or the Mac reboots mid-update, the next run (at boot)
finishes the verification or rolls back, and discards it if someone else (e.g. `umm fleet upgrade`) replaced
the binary meanwhile. A file lock stops two cycles overlapping. Log: `/usr/local/var/log/umm-updater.log`.
Nothing is shown to the Mac's user.

**Updating the updater.** The updater is the agent binary, so a new release updates both: the next cycle
runs the new code. The LaunchDaemon plist itself only changes with `install.sh` or `umm fleet
install/upgrade`. If a release ever broke the updater while the agent still heartbeats, `umm fleet upgrade`
repairs it.

**Trust summary.** Root code only runs what the release key signed, for the version the router asked for.
A compromised router can hold updates back, or (only by pinning) roll Macs back to an older signed release;
it cannot make them run unsigned code. The signing key lives only in the Actions secret and an offline
backup.

## Metrics history

The Prometheus endpoint (`/metrics`) has the live gauges and counters for an external Prometheus. The
dashboard's charts need history without one, so the router keeps its own, in the same database (SQLite or
Postgres):

- **Sampling.** Once a minute the router writes one row to `fleet_samples` (Macs by class, tasks running and
  queued, and the counters since the last sample: assignments, attempt outcomes, task-seconds) and one row
  per Mac to `mac_samples` (user CPU, our CPU, GPU, memory used, tasks running, online, yielded). Telemetry is
  averaged over the heartbeats of that minute. Each Mac is in exactly one class: offline, draining (admin,
  local opt-out or updating), yielded (yielding or cooldown), busy (running a task) or idle. Task-seconds are
  tasks running × the time since the previous sample; "compute hours" is their sum.
- **Rollup.** Rows are keyed by `(res, ts)` (plus the Mac): resolution 60 s and 900 s. After each sample the
  15-minute bucket it falls in is recomputed from its 1-minute rows (gauges `AVG`, counters `SUM`, `n` = rows
  merged), so rollups are always complete and recomputing one is harmless.
- **Queries.** `GET /v1/metrics/fleet` and `/v1/metrics/macs/{id}` read 1-minute rows while the range starts
  inside the 1-minute retention, else 15-minute rows, and merge buckets in SQL to the requested `step`
  (default about 120 points, at most 1,500). Missing buckets come back as points with only `t`, so charts show
  gaps where the router was down. `GET /v1/metrics/summary` sums the counters for 24 hours and 7 days, plus
  what has not been sampled yet.
- **Retention.** Defaults: 1-minute rows 24 hours (24 to 168), 15-minute rows 30 days, activity log 90 days,
  finished jobs kept. The config file's `retention:` section sets the defaults; admins override them at
  runtime (Settings or `PUT /v1/retention`). An hourly pass prunes metrics and events, deletes spent one-time
  enrolment tokens and, when `jobs_days` is set, deletes jobs that finished longer ago (tasks, attempts, logs
  and outputs in router storage). Size: one fleet row plus one row per Mac per minute; 100 Macs is about
  145,000 rows a day at 1 minute (pruned to 24 hours) and about 290,000 rollup rows for 30 days.

## Activity log and cleanup

`events` holds notable fleet events (enrolments, yields, quarantines, failed or cancelled jobs, update
results) and an audit trail of admin changes: keys created, revoked and deleted; Macs, jobs and install links
deleted; bulk purges and retention changes, each with the acting subject. Deletes only remove leftovers: a
key must be revoked first, an install link must be revoked, expired or used up, and a job must be finished
(`409` otherwise). Deleting a Mac revokes its credential (the row with the credential hash is gone) and
removes its metrics; its past attempts stay with their jobs.

## Failure modes

| Failure | Detection | Handling |
| --- | --- | --- |
| Task exits non-zero / missing output | agent report | failure: `failures++`, exclude that Mac for `fail_exclude`, retry after `backoff_base × 2^(n−1)` (max `backoff_max`); give up after `max_retries` |
| Task exceeds timeout | agent kills it | as a failure (`timeout`) |
| Runtime program missing or not executable | agent preflight (startup, every `preflight_interval`, `SIGHUP`) | runtime not advertised; shown as unavailable with the reason; never scheduled there |
| Task cannot execute its runtime (exit 127, exec `ENOENT`/`EACCES`) | agent report (`runtime_unavailable`, or a bare exit 127 from older agents) | Mac fault: attempt `unavailable`, no failure counted, requeued at once, Mac excluded for that task for `yield_exclude`; the runtime is unavailable on that Mac until a newer preflight reports it (agents without preflight: `legacy_fault_ttl`). After `max_mac_faults` such attempts a task's further ones count as failures |
| Agent dies silently mid-task | no heartbeat for `heartbeat_timeout` | requeue (`lost`), no failure counted; if the agent comes back and still reports the task it is told to stop |
| Mac's user goes heavy | yield policy | `yielded`, requeue at once, Mac excluded for that task for `yield_exclude` |
| Mac keeps failing tasks | `quarantine_after` consecutive failures | quarantined for `quarantine_for` (no new work); a success resets the count |
| Agent never picks up an assignment | `assign_timeout` | requeue elsewhere |
| Agent reports for an attempt it no longer owns | attempt/agent mismatch | `409`, ignored |
| Router restarts | live state is rebuilt from heartbeats | running tasks are kept for one `heartbeat_timeout` grace period, then requeued if their agent does not report them |
| Upstream task fails | dependency check | dependents `cancelled`, job `failed` |
| Status report lost on the network | agent retries 5× | if still lost: deadline + grace marks a timeout |
| New agent version does not start or heartbeat | updater waits `verify_timeout` | previous binary restored, `rolled_back` reported, that version retried after 1 h, 2 h, … 24 h |
| Updater dies or the Mac reboots mid-update | pending state in `state.json` | next run (at boot) finishes verification or rolls back |
| Release unsigned, tampered or for another version | signature / checksum / trusted comment | nothing installed, `failed` reported |

Placement uses at-least-once semantics: a task may run more than once (e.g. a partitioned agent keeps
running briefly). Tasks should be idempotent; outputs are overwritten per key.

## Scale and limits

- One router replica (`Recreate` strategy). Live agent state is in memory and the database is the source of
  truth. Metrics history adds one write transaction a minute. The tick reads all active tasks, which is fine
  for thousands of active tasks and tens to hundreds of Macs.
- SQLite uses one connection (WAL). Use Postgres for heavier use.
- Long-poll requests hold for up to 30 s, so proxies need a longer timeout (the example HTTPRoute sets
  120 s for `/v1/agent/`).

## macOS telemetry sources

| Signal | Source | Notes |
| --- | --- | --- |
| console user | `ioreg -n Root -d1` `IOConsoleUsers`, the session with `kCGSSessionOnConsoleKey=Yes` | falls back to `stat -f '%u %Su' /dev/console`, which can be owned by root while a user is logged in and locked |
| HID idle | `ioreg -c IOHIDSystem` `HIDIdleTime` | ns |
| screen locked | the on-console session's `CGSSessionScreenIsLocked`, or `IOConsoleLocked` | |
| per-process CPU/RSS | `ps -axo pid,pgid,uid,pcpu,rss` | split into user and ours |
| memory | `sysctl hw.memsize`, `vm_stat` (free + inactive + speculative + purgeable), `kern.memorystatus_vm_pressure_level` | |
| thermal | the worse of `notifyutil -g com.apple.system.thermalpressurelevel` (0 nominal, 1 moderate → fair, 2 heavy → serious, 3+ → critical) and `pmset -g therm` | the notify key is what `ProcessInfo.thermalState` reads and is current on Apple Silicon; no privileges needed. Feeds both the yield policy (`thermal`, default `serious`) and the score. Fan speed is not readable without SMC access, so it is not used |
| power | `pmset -g batt` | |
| GPU | `ioreg -r -d 1 -c AGXAccelerator` `PerformanceStatistics` → `Device Utilization %` | also `gpu-core-count` |
| disk | `df -k <work_dir>` | |

All parsers are pure functions with tests. Only the choice of collector is build-tagged
(`default_darwin.go` / `default_other.go`), so everything builds and tests on Linux and Windows.

## Open questions

- **Background QoS vs throughput.** This is why priority is adaptive. Measured on an M3 Ultra (20 P + 8 E
  cores), one 30 s 1080p H.264 chunk: `hevc_videotoolbox` 4.1–4.4 s at background vs 1.4 s at utility
  (1.4 s unwrapped; the H.264 decode runs on the CPU); `libx264 -preset medium` 15.4 s vs 2.1 s (1.9 s
  unwrapped). VideoToolbox works at both priorities. Open: whether a demoted task should also be
  preferred for reassignment when another Mac is idle.
- **VideoToolbox's helper process.** Hardware encodes run partly in `VTEncoderXPCService`, a separate
  process launched for the calling user and outside the task's process group. Under `_umm` it counts as
  ours by uid; an agent running as the console user counts it as user load (small: about 13 % of one core
  per encode in the trial).
- **VideoToolbox/Metal from a LaunchDaemon user** without a GUI session is expected to work for
  compute and hardware encode, but needs confirming on hardware.
- **User-facing control** beyond `pause_file`, such as a menu-bar item that lets the Mac's owner pause
  for an hour.
- **High availability** of the router (leader election) is out of scope for now.
