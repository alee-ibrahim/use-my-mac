# Using use-my-mac

This guide is for people who submit work to a use-my-mac router and for admins who run one. For how the
router decides where work runs, see [Design](design.md). For rolling the agent out to many Macs, see
[Fleet install](fleet.md).

## Concepts

| Term | Meaning |
| --- | --- |
| **Router** | `umm-server`. Holds the REST API, the scheduler, the dashboard and (optionally) file storage. |
| **Mac** | An Apple Silicon Mac running `umm-agent`. It connects out to the router; it never accepts inbound connections. |
| **Runtime** | A name the Mac's admin has allowlisted in `agent.yaml`, mapped to a fixed program, e.g. `ffmpeg: ["/opt/homebrew/bin/ffmpeg"]`. A task picks a runtime and adds arguments. It can never choose the executable. |
| **Job** | What you submit: a name, optional labels and a set of tasks. A job is a small DAG. |
| **Task** | One program run: `runtime`, `args`, `inputs`, `outputs`, `resources`, `depends_on`. |
| **Attempt** | One try of a task on one Mac. A task can have several (retries, yields, lost Macs). |
| **Yield** | The Mac's own user started heavy work, so the agent stopped our task. The router moves it to another Mac straight away. A yield is not a failure. |

Job states: `queued`, `running`, `succeeded`, `failed`, `cancelled`. Task states: `blocked` (waiting on
dependencies), `pending` (waiting for a Mac), `assigned`, `running`, `succeeded`, `failed`, `cancelled`.

Tasks may run more than once (at-least-once). Write them so that a re-run gives the same result.

## Get access

Everything except the dashboard uses an **API key** (`umm_key_...`), sent as
`Authorization: Bearer umm_key_...`. An admin creates keys in the dashboard (**API keys**) or with the CLI:

```sh
umm keys create my-app              # role app (default)
umm keys create reporting -role viewer
```

| Role | Can |
| --- | --- |
| `app` | submit jobs, upload inputs, see and cancel **its own** jobs |
| `viewer` | read everything (jobs, Macs, settings); no changes |
| `admin` | everything, including Macs, enrolment tokens, API keys and settings |

The key is shown once. Keep it in a secret store or a file, never on a command line or in a repo.

The dashboard uses single sign-on (OIDC) when the router has it configured; admin and viewer roles come
from your groups. Admin and viewer API keys can also sign in to the dashboard. App keys cannot.

## Install the CLI

Download `umm` for your OS from the project's
[GitHub releases](https://github.com/alee-ibrahim/use-my-mac/releases) (`umm-darwin-arm64`,
`umm-linux-amd64`, `umm-windows-amd64.exe`, ...). Check it against `SHA256SUMS`, make it executable and put
it on your `PATH`. Then point it at the router:

```sh
export UMM_SERVER=https://umm.example.com
# The key: UMM_API_KEY, a file named by UMM_API_KEY_FILE, or ~/.config/umm/api-key
mkdir -p ~/.config/umm && chmod 700 ~/.config/umm
$EDITOR ~/.config/umm/api-key       # paste the key, save
umm macs                            # quick check
```

| Command | Does |
| --- | --- |
| `umm submit FILE [-wait]` | submit a job spec (YAML or JSON); `file:` inputs are uploaded first |
| `umm status [JOB]` | recent jobs, or one job and its tasks |
| `umm logs TASK [-attempt N]` | task logs (all attempts by default) |
| `umm download TASK OUTPUT [-o FILE]` | download an output |
| `umm cancel JOB` | cancel a job |
| `umm macs` | Macs with live telemetry |
| `umm drain MAC [-off]` | stop sending new work to a Mac (admin) |
| `umm enrol [-ttl-hours N]` | one-time enrolment token for a new Mac (admin) |
| `umm keys list` / `create NAME [-role R]` / `revoke ID` | API keys (admin) |
| `umm fleet ...` | install and manage agents over SSH (admin), see [Fleet install](fleet.md) |

## Submit a job

### With the CLI

```yaml
# job.yaml
name: encode
tasks:
  - key: part-1
    runtime: ffmpeg
    args: ["-i", "{in:src}", "-c:v", "hevc_videotoolbox", "-b:v", "8M", "{out:enc.mp4}"]
    inputs:  [{name: src, file: ./part-1.mov}]   # uploaded by the CLI
    outputs: [{name: enc.mp4}]
    resources: {ram_mb: 2048, gpu: true, expected_seconds: 120}
```

```sh
umm submit job.yaml -wait      # prints the job ID, then progress
umm status JOB_ID              # task IDs and states
umm logs TASK_ID
umm download TASK_ID enc.mp4
```

### Task fields

| Field | Meaning |
| --- | --- |
| `key` | unique name inside the job; other tasks refer to it |
| `runtime` | an allowlisted runtime name (see [Runtimes](#runtimes)) |
| `args` | appended to the runtime's fixed argv. Placeholders: `{in:NAME}` input path, `{out:NAME}` where output NAME must be written, `{workdir}` the task's private directory |
| `env` | extra environment variables. `PATH`, `HOME`, `TMPDIR`, `UMM_*`, `DYLD_*` and `LD_*` are not allowed |
| `inputs` | files placed in `in/` before the task starts. Each has a `name` and exactly one of `url`, `blob` or `from` (the CLI also accepts `file`) |
| `outputs` | files the task must write to `out/NAME`. `optional: true` allows a missing file; `url` sends it straight to your own PUT URL |
| `resources` | `ram_mb`, `gpu`, `expected_seconds`, `disk_mb`, `labels` (all must match the Mac's labels) |
| `timeout_seconds` | limit for one attempt (router default if 0) |
| `max_retries` | failed attempts retried before giving up (router default if unset). Yields and lost Macs do not count |
| `depends_on` | task keys that must succeed first |

A task's standard output and error are its logs. Small results can simply be printed.

### With the REST API

Any language works. Upload inputs, submit, poll, download:

```sh
curl -sS -X PUT -H "Authorization: Bearer $UMM_API_KEY" \
  --data-binary @page1.png "$UMM_SERVER/v1/blobs/myapp/run-42/page1.png"

curl -sS -X POST -H "Authorization: Bearer $UMM_API_KEY" -H "Content-Type: application/json" \
  "$UMM_SERVER/v1/jobs" -d '{
    "name": "ocr run-42",
    "tasks": [{
      "key": "p1", "runtime": "ocr",
      "args": ["{in:img}", "{out:text.txt}"],
      "inputs": [{"name": "img", "blob": "myapp/run-42/page1.png"}],
      "outputs": [{"name": "text.txt"}]
    }]
  }'

curl -sS -H "Authorization: Bearer $UMM_API_KEY" "$UMM_SERVER/v1/jobs/JOB_ID"
curl -sS -H "Authorization: Bearer $UMM_API_KEY" -o text.txt "$UMM_SERVER/v1/tasks/TASK_ID/outputs/text.txt"
```

`GET /v1/jobs/{id}` lists the tasks with their IDs. A full Python example (batched OCR with a merge step) is
in `examples/ocr-batch/` in the repository.

### With the Go SDK

```go
import (
    "github.com/alee-ibrahim/use-my-mac/pkg/api"
    "github.com/alee-ibrahim/use-my-mac/pkg/client"
)

c := client.New(os.Getenv("UMM_SERVER"), os.Getenv("UMM_API_KEY"))

f, _ := os.Open("part-1.mov")
st, _ := f.Stat()
_ = c.UploadBlob(ctx, "myapp/part-1.mov", f, st.Size())

job, err := c.SubmitJob(ctx, api.JobSpec{Name: "encode", Tasks: []api.TaskSpec{{
    Key: "part-1", Runtime: "ffmpeg",
    Args:      []string{"-i", "{in:src}", "-c:v", "hevc_videotoolbox", "{out:enc.mp4}"},
    Inputs:    []api.Input{{Name: "src", Blob: "myapp/part-1.mov"}},
    Outputs:   []api.Output{{Name: "enc.mp4"}},
    Resources: api.Resources{RAMMB: 2048, GPU: true},
}}})

final, err := c.WaitJob(ctx, job.ID, 3*time.Second, nil)
out, _ := os.Create("enc.mp4")
_, err = c.DownloadOutput(ctx, final.Tasks[0].ID, "enc.mp4", out)
```

The client also has `Job`, `Jobs`, `CancelJob`, `Task`, `Logs`, `Macs` and the admin calls (`PatchMac`,
`CreateEnrolmentToken`, `CreateAPIKey`, `APIKeys`, `RevokeAPIKey`). `examples/video-transcode/` in the
repository is a complete map/reduce program.

## Map/reduce jobs

Your app does the splitting. Submit one task per piece (the map), then a task that takes their outputs (the
reduce). An input with `from: {task, output}` points at another task's output and adds the dependency for
you:

```yaml
name: transcode
tasks:
  - key: chunk-0
    runtime: ffmpeg
    args: ["-i", "{in:src}", "-c:v", "hevc_videotoolbox", "{out:part.ts}"]
    inputs:  [{name: src, file: ./chunk-0.mov}]
    outputs: [{name: part.ts}]
    resources: {gpu: true, ram_mb: 2048}
  - key: chunk-1
    runtime: ffmpeg
    args: ["-i", "{in:src}", "-c:v", "hevc_videotoolbox", "{out:part.ts}"]
    inputs:  [{name: src, file: ./chunk-1.mov}]
    outputs: [{name: part.ts}]
    resources: {gpu: true, ram_mb: 2048}
  - key: concat
    runtime: concat
    args: ["{out:final.mp4}", "{in:p0}", "{in:p1}"]
    inputs:
      - {name: p0, from: {task: chunk-0, output: part.ts}}
      - {name: p1, from: {task: chunk-1, output: part.ts}}
    outputs: [{name: final.mp4}]
```

The map tasks spread across the Macs. The reduce runs when all of them have succeeded. If a map task fails
for good, its dependents are cancelled and the job ends `failed`. Cycles and unknown references are rejected
when you submit.

## Runtimes

Each Mac lists what it may run in `/usr/local/etc/umm/agent.yaml` (or the `runtimes` key of a `umm fleet`
inventory):

```yaml
runtimes:
  ffmpeg: ["/opt/homebrew/bin/ffmpeg"]
  ocr:    ["/usr/bin/python3", "/opt/ocr/run.py"]
```

- The value is a fixed argv prefix with an absolute path. The task's `args` are appended to it.
- Allowlist specific scripts, not bare interpreters: `python3` alone would let a task run any code.
- The router only places a task on Macs that have its runtime. `umm macs` and the dashboard show each Mac's
  runtimes.
- Tasks run as the hidden non-admin `_umm` user at background priority (`taskpolicy -b`), in a fresh private
  work dir that is deleted afterwards. Install the tools they need where that user can read them.

## Storage

Inputs and outputs never go through the agent's connection to the router. Agents get short-lived URLs.

| Option | Use when | How |
| --- | --- | --- |
| Router storage, `local` backend (default) | small to medium files, no extra infrastructure | upload with `PUT /v1/blobs/KEY` (or `file:` in the CLI), refer to it as `blob: KEY`. Outputs are kept at `jobs/<job>/<task-key>/<output>` |
| Router storage, `s3` backend | big files, many Macs | same API; the router hands agents presigned URLs for any S3-compatible store |
| Your own URLs | the data already lives somewhere | `inputs[].url` (GET) and `outputs[].url` (PUT), e.g. presigned URLs from your own store |
| Another task's output | map/reduce | `from: {task, output}` |

Blob keys are relative paths: no leading `/`, no `..`. Outputs are overwritten per key, so re-runs are safe.

## API reference

All endpoints take and return JSON unless noted. Authenticate with `Authorization: Bearer umm_key_...`.

| Method and path | Role | Does |
| --- | --- | --- |
| `GET /v1/me` | any | who you are and your role |
| `POST /v1/jobs` | app, admin | submit a job (body: job spec); returns the job (`201`) |
| `GET /v1/jobs?state=&limit=` | any | list jobs (apps see only their own) |
| `GET /v1/jobs/{id}` | any | a job, its tasks and state counts |
| `POST /v1/jobs/{id}/cancel` | app, admin | cancel a job |
| `GET /v1/tasks/{id}` | any | a task with its attempts and placement scores |
| `GET /v1/tasks/{id}/logs?attempt=N` | any | logs as plain text (all attempts when `attempt` is 0 or missing) |
| `GET /v1/tasks/{id}/outputs/{name}` | any | download an output |
| `PUT /v1/blobs/{key}` | app, admin | upload an input (raw body) |
| `GET /v1/blobs/{key}` | app, admin | download a blob |
| `GET /v1/macs` | any | Macs with state and live telemetry |
| `PATCH /v1/macs/{id}` | admin | `{"drain": true}` or `{"opted_out": true}` |
| `DELETE /v1/macs/{id}` | admin | remove a Mac and revoke its credential |
| `POST /v1/enrolment-tokens` | admin | one-time token for a new Mac |
| `GET` / `POST /v1/api-keys`, `DELETE /v1/api-keys/{id}` | admin | list, create (`{"name", "role"}`) and revoke API keys |
| `GET /v1/settings` | viewer, admin | scheduler weights and limits |
| `PUT /v1/settings` | admin | change them at runtime |
| `GET /v1/docs`, `GET /v1/docs/{slug}` | public | these docs (index, raw Markdown) |
| `GET /healthz` | public | liveness |
| `GET /metrics` | public | Prometheus metrics |

Apps that can see a job can also read its tasks, logs and outputs. Errors come back as
`{"error": "message"}` with a 4xx or 5xx status.

## Enrolling Macs

Requirements: Apple Silicon, a recent macOS, outbound HTTPS to the router, and an admin account on the Mac.

**One Mac by hand.** Download `umm-agent-darwin-arm64.tar.gz` from the release, copy it to the Mac, unpack
it and run:

```sh
sudo UMM_SERVER=https://umm.example.com sh install.sh
```

Paste the one-time token when asked. Get it from the dashboard (**Macs**, then **Add a Mac**) or with
`umm enrol`. The script creates the `_umm` user, installs the agent, writes `/usr/local/etc/umm/agent.yaml`
and loads a LaunchDaemon. Edit `runtimes` in `agent.yaml`, then restart the daemon.

**Many Macs.** Use `umm fleet` from an admin workstation over SSH: one `bootstrap` per Mac (the only step
that needs the sudo password), then `install`, `upgrade`, `status` and `uninstall` in parallel. See
[Fleet install](fleet.md).

**Day to day.**

- Anyone with admin rights on a Mac can pause it: `sudo touch /usr/local/etc/umm/paused` (remove the file
  to resume).
- Admins can **drain** a Mac (running tasks finish, no new work) or **opt it out** (tasks stop and move now)
  from the dashboard or with `umm drain`.
- `umm-agent telemetry` on a Mac prints what the agent sees: console user, idle time, load, memory, thermal,
  power.
- Deleting a Mac in the dashboard revokes its credential. Re-enrol it with a new token.

## Troubleshooting

| Symptom | Look at | Likely cause |
| --- | --- | --- |
| Task stays `pending` | the task's **Placement** tab in the dashboard: every Mac and why it was skipped | no Mac has the runtime, not enough RAM or disk, labels don't match, all Macs busy, yielding, drained or offline |
| Task keeps moving between Macs | attempts with outcome `yielded` | the Macs' users are busy. Normal; it runs when a Mac is quiet. Long tasks prefer idle Macs if you set `expected_seconds` |
| Task `failed` | `umm logs TASK` (all attempts) | the program exited non-zero, timed out, or did not write a declared output to `out/NAME` |
| `401 Unauthorized` | the key | missing, mistyped or revoked key; the header must be `Authorization: Bearer umm_key_...` |
| `403` | the error text names your role | an app key used for an admin call, or a viewer trying to change something |
| `400` on submit | the error text | unknown field, duplicate task key, cycle, an input with no or several sources, or a disallowed `env` name |
| Mac shows offline | `umm-agent telemetry` and the agent log on the Mac | network or proxy to the router, a revoked credential, or the daemon is not loaded |
| Mac never `ready` | the Mac's state and reason in the dashboard | paused (`paused` file), drained, quarantined after repeated failures, on battery, or its user is busy |
| Program not found or permission denied | the runtime path in `agent.yaml` | the path must be absolute and readable and executable by `_umm` |
