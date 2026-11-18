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
runtime, has non-matching labels or is at `max_tasks`. Resource reasons: the task needs more RAM than the Mac
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
  a failure. The task is excluded from that Mac for `yield_exclude`.
- While not `ready` the agent stops polling for work, and the router withdraws any assignment the agent
  had not yet picked up.
- **Background priority:** every task runs under `taskpolicy -b` (or `-c utility` + `nice`, configurable),
  so even before a yield the scheduler on the Mac favours the user's apps. On Apple Silicon, background QoS
  also keeps CPU work on efficiency cores. That is the most polite choice but it lowers throughput (see
  open questions).
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

## Security model

- **No inbound ports on Macs.** Agents only make outbound HTTPS requests (heartbeat, long-poll, status,
  logs, blob URLs).
- **Enrolment:** an admin mints a one-time enrolment token (24 h by default, stored hashed, consumed
  atomically). The agent exchanges it for a per-Mac credential (`umm_agt_…`, 256-bit). Only its SHA-256 is
  stored server-side. On the Mac it lives in a 0600 file owned by `_umm`. Deleting the Mac in the dashboard
  revokes it.
- **API keys** (`umm_key_…`) are hashed at rest and have a role: `app` (submit, and see only its own
  jobs), `viewer` (read everything) or `admin`.
- **Dashboard:** OIDC authorization-code flow with PKCE and nonce. Roles come from a groups claim and/or
  an app-roles claim (`admin_groups` and `viewer_groups`; admin wins). Users in neither are denied unless
  `default_role: viewer`. Microsoft Entra ID **groups overage** (no groups in the token) is detected and
  refused with a clear error. Use app roles, or "groups assigned to the application". Sessions are
  HMAC-signed, HttpOnly, SameSite=Lax cookies (Secure behind HTTPS). Cookie-authenticated writes require an
  `X-UMM-CSRF` header. API-key sign-in exists for setups without SSO and as break-glass access (admin and
  viewer keys only).
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

## Failure modes

| Failure | Detection | Handling |
| --- | --- | --- |
| Task exits non-zero / missing output | agent report | failure: `failures++`, exclude that Mac for `fail_exclude`, retry after `backoff_base × 2^(n−1)` (max `backoff_max`); give up after `max_retries` |
| Task exceeds timeout | agent kills it | as a failure (`timeout`) |
| Agent dies silently mid-task | no heartbeat for `heartbeat_timeout` | requeue (`lost`), no failure counted; if the agent comes back and still reports the task it is told to stop |
| Mac's user goes heavy | yield policy | `yielded`, requeue at once, Mac excluded for that task for `yield_exclude` |
| Mac keeps failing tasks | `quarantine_after` consecutive failures | quarantined for `quarantine_for` (no new work); a success resets the count |
| Agent never picks up an assignment | `assign_timeout` | requeue elsewhere |
| Agent reports for an attempt it no longer owns | attempt/agent mismatch | `409`, ignored |
| Router restarts | live state is rebuilt from heartbeats | running tasks are kept for one `heartbeat_timeout` grace period, then requeued if their agent does not report them |
| Upstream task fails | dependency check | dependents `cancelled`, job `failed` |
| Status report lost on the network | agent retries 5× | if still lost: deadline + grace marks a timeout |

Placement uses at-least-once semantics: a task may run more than once (e.g. a partitioned agent keeps
running briefly). Tasks should be idempotent; outputs are overwritten per key.

## Scale and limits

- One router replica (`Recreate` strategy). Live agent state is in memory and the database is the source of
  truth. The tick reads all active tasks, which is fine for thousands of active tasks and tens to hundreds
  of Macs.
- SQLite uses one connection (WAL). Use Postgres for heavier use.
- Long-poll requests hold for up to 30 s, so proxies need a longer timeout (the example HTTPRoute sets
  120 s for `/v1/agent/`).

## macOS telemetry sources

| Signal | Source | Notes |
| --- | --- | --- |
| console user | `stat -f '%u %Su' /dev/console` | root or `_mbsetupuser` = nobody |
| HID idle | `ioreg -c IOHIDSystem` `HIDIdleTime` | ns |
| screen locked | `ioreg -n Root -d1` `CGSSessionScreenIsLocked` / `IOConsoleLocked` | |
| per-process CPU/RSS | `ps -axo pid,pgid,uid,pcpu,rss` | split into user and ours |
| memory | `sysctl hw.memsize`, `vm_stat` (free + inactive + speculative + purgeable), `kern.memorystatus_vm_pressure_level` | |
| thermal | `pmset -g therm` | sparse on Apple Silicon (see open questions) |
| power | `pmset -g batt` | |
| GPU | `ioreg -r -d 1 -c AGXAccelerator` `PerformanceStatistics` → `Device Utilization %` | also `gpu-core-count` |
| disk | `df -k <work_dir>` | |

All parsers are pure functions with tests. Only the choice of collector is build-tagged
(`default_darwin.go` / `default_other.go`), so everything builds and tests on Linux and Windows.

## Open questions

- **Background QoS vs throughput.** `taskpolicy -b` confines CPU work to efficiency cores on Apple
  Silicon. That is safest for users but slow for CPU-heavy tasks on idle Macs. A likely better policy is
  `utility` when nobody is logged in or the screen is locked, and `background` otherwise.
- **Thermal on Apple Silicon.** `pmset -g therm` rarely reports anything. A tiny Swift helper reading
  `ProcessInfo.thermalState` would be accurate.
- **VideoToolbox/Metal from a LaunchDaemon user** without a GUI session is expected to work for
  compute and hardware encode, but needs confirming on hardware.
- **User-facing control** beyond `pause_file`, such as a menu-bar item that lets the Mac's owner pause
  for an hour.
- **High availability** of the router (leader election) is out of scope for now.
