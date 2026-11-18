# use-my-mac

**Turn the Apple Silicon Macs you already have into a compute pool, without getting in anyone's way.**

`use-my-mac` (CLI: `umm`) is a small router plus an agent for macOS. Apps submit work over a REST API.
The router places each task on the best Mac for it: idle or locked Macs first, Macs that fit the
memory and GPU needs, AC power and cool ones over battery and hot ones. Work is spread across the fleet.
Staff Macs with someone logged in still count. When that person starts heavy work of their own, the agent
stops our task within seconds and the router moves it to another Mac.

- **Generic.** A task is an allowlisted program, its arguments, input files, output files and resource
  hints. A job is a set of tasks with optional dependencies (map, then reduce). The core has no video, OCR
  or ML code.
- **Native, not containers.** Tasks run as plain macOS processes, so they keep Metal, VideoToolbox and the
  Neural Engine. They run at utility QoS while nobody is using the Mac and at background QoS (efficiency
  cores) while someone is, switching in place, in a private work dir as a dedicated non-admin user.
- **Polite.** The agent watches the console user, HID idle time, screen lock, the user's own CPU (our
  processes excluded), memory pressure, thermal state and power source. Being logged in doesn't stop work.
  Sustained heavy use by the user does.
- **No inbound ports on Macs.** Agents connect outbound and long-poll. Enrolment uses a one-time token that
  is exchanged for a per-Mac credential.
- **Self-updating agents.** A root updater on each Mac follows the version the router sets (with canaries and
  a pause switch), installs only releases signed with the project's minisign key, waits for running tasks
  and rolls back automatically if the new agent does not report in.
- **Zero extra infra.** One Go binary with SQLite and local file storage. When you need more, add Postgres
  and any S3-compatible store (agents only ever get short-lived presigned URLs).
- **Observable.** Prometheus metrics, a dashboard (React + shadcn/ui) with live Mac telemetry, job progress,
  per-attempt history, logs and the reason each Mac was picked or skipped. Sign-in uses OIDC (Microsoft Entra
  ID preset included) with admin and viewer roles mapped from groups.

> Status: early. The router, scheduler and agent are covered by unit and integration tests on Linux, and the
> agent cross-compiles for `darwin/arm64`. The agent and router have had a first trial on a real Mac
> (M3 Ultra, macOS 27) running as the logged-in user; the LaunchDaemon install is still untested. See
> [What's verified](#whats-verified).

## How it works

```mermaid
flowchart LR
  subgraph Apps
    A1[your app / umm CLI / Go SDK]
  end
  subgraph Router["umm-server (Kubernetes or any host)"]
    API[REST API + dashboard]
    S[Scheduler: score + place,<br/>retry, reassign]
    DB[(SQLite / Postgres)]
    ST[(Blob storage:<br/>local disk or S3)]
    M[/metrics/]
    API --> S --> DB
    API --> ST
  end
  subgraph Fleet["Macs (outbound only)"]
    G1[umm-agent<br/>telemetry + yield policy]
    G2[umm-agent]
    G3[umm-agent]
  end
  A1 -- "submit job, fetch outputs" --> API
  G1 & G2 & G3 -- "heartbeat + long-poll" --> API
  G1 & G2 & G3 -- "short-lived signed URLs" --> ST
```

1. An app submits a **job**: tasks with `runtime`, `args`, `inputs`, `outputs`, `resources` and
   `depends_on`.
2. The scheduler scores every Mac for every runnable task and assigns the best one. Hard filters cover
   offline, draining, quarantined, wrong runtime, not enough RAM or disk, critical thermal state and battery.
3. The agent picks up the task on its next long-poll. It downloads inputs into `in/` and runs
   `<allowlisted program> <args>` under `taskpolicy` (see adaptive priority), streaming logs as it goes. Then it uploads `out/*`.
4. If the Mac's user goes heavy, the agent stops the task (SIGTERM, then SIGKILL) and reports `yielded`. The
   router reassigns it straight away. A yield doesn't count as a failure. Failures are retried with
   backoff on a different Mac, and lost heartbeats trigger reassignment.
5. When all map tasks succeed, the reduce task runs with their outputs as inputs.

Details: [docs/design.md](docs/design.md). A user guide (submitting jobs, runtimes, storage, API reference,
troubleshooting) is in [docs/usage.md](docs/usage.md); the router also serves all of these docs at `/docs`.

## Quickstart (local)

Requirements: Go 1.26+, Node 24+ (only for the dashboard).

```sh
make web build                 # dashboard + bin/umm-server, bin/umm, bin/umm-agent
export UMM_SECRET_KEY=$(openssl rand -hex 32)
./bin/umm-server admin-key create -config deploy/dev.yaml   # prints an admin key once
make run-dev                   # http://localhost:8080
```

Sign in to the dashboard with the admin key (API-key sign-in is on when OIDC is not configured; see
`auth` in `deploy/server.example.yaml`). Then from another shell:

```sh
export UMM_SERVER=http://localhost:8080
export UMM_API_KEY=umm_key_...           # the admin key (or create an app key: umm keys create my-app)
./bin/umm macs
```

### Add a Mac

With an https router, the quickest way is one command. In the dashboard open **Macs → Add Mac → One
command** (or run `umm enrol-command`), copy the line and run it in Terminal on the Mac:

```sh
curl -fsSL https://umm.example.com/i/umm_ins_... | sudo sh
```

It verifies the signed agent release, installs and enrols the agent and waits for its first heartbeat. The
link is short-lived, single use by default and revocable. See
[Add a Mac with one command](docs/usage.md#add-a-mac-with-one-command). By hand instead:

```sh
make dist-mac                            # dist/umm-agent-darwin-arm64.tar.gz
# copy it to the Mac, unpack, then on the Mac:
sudo UMM_SERVER=https://umm.example.com sh install.sh
# paste the one-time token from the dashboard (Macs -> Add a Mac) or `umm enrol`
```

`install.sh` creates the hidden `_umm` service user and installs `/usr/local/bin/umm-agent`. It writes
`/usr/local/etc/umm/agent.yaml` (edit the `runtimes` allowlist there) and loads a LaunchDaemon. Anyone with
admin rights on the Mac can pause it with `sudo touch /usr/local/etc/umm/paused`. Check what the agent sees
with `umm-agent telemetry`.

### Add many Macs (`umm fleet`)

To roll out across a fleet from an admin workstation (Windows, macOS or Linux) over SSH, without touching
each Mac by hand, use `umm fleet`. It reads a YAML inventory, installs a narrowly-scoped sudoers drop-in once
per Mac (`bootstrap`), then installs/enrols/upgrades/removes agents in parallel. Agents never get SSH keys,
and no secret is ever passed on the command line.

```sh
make agent-darwin                                           # build the bundle
umm fleet bootstrap -inventory fleet.yaml -sudo-password-file ~/.umm/sudo.txt   # one-time, per Mac
umm fleet install   -inventory fleet.yaml -admin-key-file ~/.umm/admin.key      # install + enrol + verify
umm fleet status    -inventory fleet.yaml
```

Details, security model and the quiet-install notes are in [docs/fleet.md](docs/fleet.md). Example inventory:
[`deploy/mac/fleet.example.yaml`](deploy/mac/fleet.example.yaml).

### Submit work

```yaml
# job.yaml
name: encode
tasks:
  - key: part-1
    runtime: ffmpeg
    args: ["-i", "{in:src}", "-c:v", "hevc_videotoolbox", "-b:v", "8M", "{out:enc.mp4}"]
    inputs:  [{name: src, file: ./part-1.mov}]   # the CLI uploads local files
    outputs: [{name: enc.mp4}]
    resources: {ram_mb: 2048, gpu: true, expected_seconds: 120}
```

```sh
umm submit job.yaml -wait
umm status JOB_ID
umm download TASK_ID enc.mp4
```

More examples, including map/reduce video encoding with the Go SDK and batched OCR over plain REST, are in
[`examples/`](examples/).

## Deploy

- **Kubernetes:** Helm chart in [`deploy/helm/use-my-mac`](deploy/helm/use-my-mac). It runs a single
  replica on a PVC with optional external Postgres, S3 storage, a Gateway API `HTTPRoute` and a
  `ServiceMonitor`. Secrets come from an existing Secret and are never rendered by the chart.
- **Container:** `ghcr.io/alee-ibrahim/use-my-mac:<version>` (linux/amd64 and linux/arm64), or
  `docker build -t use-my-mac .`. A distroless, non-root image with the dashboard included.
- **Releases:** each [GitHub release](https://github.com/alee-ibrahim/use-my-mac/releases) has the Mac
  agent bundle (`umm-agent-darwin-arm64.tar.gz`), the `umm` CLI for macOS, Linux and Windows, a standalone
  router with the dashboard (`umm-server-<os>-<arch>.tar.gz`), `SHA256SUMS` and its minisign signature
  `SHA256SUMS.minisig` (public key: [`internal/selfupdate/release-signing.pub`](internal/selfupdate/release-signing.pub)).
- **Config reference:** [`deploy/server.example.yaml`](deploy/server.example.yaml),
  [`deploy/mac/agent.example.yaml`](deploy/mac/agent.example.yaml).

## API at a glance

| | |
| --- | --- |
| `POST /v1/jobs` | submit a job (app/admin) |
| `GET /v1/jobs`, `GET /v1/jobs/{id}` | list / inspect (apps only see their own jobs) |
| `POST /v1/jobs/{id}/cancel` | cancel |
| `GET /v1/tasks/{id}` | attempts, placement scores |
| `GET /v1/tasks/{id}/logs` | logs (all attempts) |
| `GET /v1/tasks/{id}/outputs/{name}` | download an output |
| `PUT /v1/blobs/{key}` | upload an input |
| `GET /v1/macs`, `PATCH /v1/macs/{id}` | fleet, drain / opt-out (admin) |
| `POST /v1/enrolment-tokens` | one-time Mac enrolment token (admin) |
| `GET/POST/DELETE /v1/api-keys` | app keys (admin) |
| `GET/PUT /v1/settings` | scheduler weights and limits |
| `GET /metrics` | Prometheus |
| `/docs` | these docs, rendered in the dashboard (no login); raw Markdown at `GET /v1/docs/{slug}` |

Authenticate with `Authorization: Bearer umm_key_...`.

## What's verified

| Area | Status |
| --- | --- |
| Scorer, yield state machine, retry/backoff, quarantine, DAG + reduce, storage (local + S3 SigV4 against AWS's test vector), OIDC role mapping | unit tests |
| Router + real agents running real processes, with simulated telemetry: a user goes heavy mid-task and the task moves to another Mac; map/reduce completes across two Macs; a crash retries on another Mac; auth boundaries | integration tests |
| SQLite and Postgres stores | tests (Postgres in CI) |
| macOS collectors (`ioreg`, `ps`, `vm_stat`, `pmset`, `sysctl`, `df`) | checked field by field against the tools on a Mac Studio (M3 Ultra, macOS 27); real output kept as test fixtures |
| Agent + router on a Mac as the logged-in user: enrolment, map/reduce video encode with `hevc_videotoolbox` under `taskpolicy -b`, yield on real user CPU load, requeue, cooldown, re-run | trialled on one Mac |
| `umm fleet`: inventory parsing, sudoers rendering, secret handling (no secrets in argv/logs); SSH transport, sftp staging, sudo-over-stdin, real `visudo -cf` validation and NOPASSWD scoping against an OpenSSH container | unit + integration tests (Docker) |
| `install.sh`, `umm fleet` on a real Mac, LaunchDaemon, `_umm` user, `sandbox-exec`, VideoToolbox from a daemon user | **not yet run on a Mac** |
| OIDC login against a live Entra tenant | **not yet tested** |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Licensed under the [Apache License 2.0](LICENSE).
