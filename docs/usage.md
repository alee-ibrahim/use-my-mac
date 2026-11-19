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
from your groups and the rules admins manage under **Settings > Access** (see
[Dashboard roles](#dashboard-roles)). Admin and viewer API keys can also sign in to the dashboard when
API-key sign-in is allowed. App keys cannot.

### Dashboard roles

Who gets which role after single sign-on comes from two sets of rules:

- **Config rules** (`oidc.admin_groups`, `oidc.viewer_groups`, `oidc.default_role` in the router config or
  Helm values). They are shown as locked "from config" and cannot be changed or removed in the dashboard.
  A config group's role is a floor: no dashboard rule can lower it, so the configured admins can never be
  locked out.
- **Dashboard rules**, added by admins under **Settings > Access** or with `umm access`. They map a group
  ID (or app-role value) or a user (subject, object ID, email or UPN) to `admin` or `viewer`, and set the
  default role for everyone else (`deny` or `viewer`).

Precedence: a **user rule** beats **group rules** (the highest role among matching groups wins), which beat
the **default role**. The default is the higher of the config and dashboard defaults. Config group roles
are applied last as a floor.

Roles are re-evaluated on every request, so a change applies on the user's next click; no sign-out is
needed. Group membership is as of the user's last sign-in. The **Users** list shows everyone who signed in
(including people who were turned away), when, their effective role and which rule decided it; pick a role
there to give that person their own user rule. An admin cannot remove their own admin role if no other
signed-in user would still be admin. Every change is written to an audit log (who, when, what).

With `default_role: viewer` everyone who can sign in sees the dashboard read-only. To limit who can sign in
at all, require assignment on the identity provider's app (Microsoft Entra ID: Enterprise applications >
the app > Properties > **Assignment required** = Yes, then assign the users or groups).

```sh
umm access rules                                  # config (locked) and dashboard rules, default role
umm access users                                  # who signed in: role and why
umm access set group 0000aaaa-... admin -name Ops # group rule (with a display name)
umm access set user someone@example.com viewer    # user rule by email/UPN/subject/object ID
umm access user SUBJECT admin|viewer|none         # a signed-in user's own rule (none removes it)
umm access default deny|viewer
umm access rm RULE_ID
umm access log                                    # audit log
```

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
| `umm submit FILE [-wait] [-part-size 8MiB] [-concurrency 4]` | submit a job spec (YAML or JSON); `file:` inputs are uploaded first (in parallel parts, with progress) |
| `umm upload FILE [KEY] [-part-size 8MiB] [-concurrency 4] [-resume ID]` | upload an input blob in parallel parts; prints the key to use as `blob:` |
| `umm watch JOB` | follow a job live: state, percent and phase until it finishes |
| `umm status [JOB]` | recent jobs, or one job and its tasks |
| `umm logs TASK [-attempt N]` | task logs (all attempts by default) |
| `umm download TASK OUTPUT [-o FILE] [-concurrency 4]` | download an output (parallel byte ranges, with progress) |
| `umm cancel JOB` | cancel a job |
| `umm macs` | Macs with live telemetry |
| `umm drain MAC [-off]` | stop sending new work to a Mac (admin) |
| `umm rename MAC NAME` | rename a Mac on the router (admin) |
| `umm enrol [-ttl-hours N]` | one-time enrolment token for a new Mac (admin) |
| `umm keys list` / `create NAME [-role R]` / `revoke ID` | API keys (admin) |
| `umm access rules` / `users` / `set` / `user` / `rm` / `default` / `name` / `log` | dashboard roles for single-sign-on users (admin), see [Dashboard roles](#dashboard-roles) |
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
umm submit job.yaml -wait      # prints the job ID, then live progress
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
| `resources` | `ram_mb`, `gpu`, `expected_seconds`, `disk_mb` (scratch space), `disk_estimate_mb` (total work space: inputs, outputs and scratch; default twice the inputs plus `disk_mb`), `labels` (all must match the Mac's labels) |
| `timeout_seconds` | limit for one attempt (router default if 0) |
| `max_retries` | failed attempts retried before giving up (router default if unset). Yields and lost Macs do not count |
| `depends_on` | task keys that must succeed first |

A task's standard output and error are its logs. Small results can simply be printed.

### Progress

While a task runs, its Mac reports what it is doing about once a second: `downloading` its inputs, `running`,
or `uploading` its outputs, with bytes and speed for transfers. The task itself can say how far it is, in
any of these ways:

- print a line `UMM_PROGRESS=42` (0 to 100) on standard output or error;
- write a number from 0 to 100 to the file named by `$UMM_PROGRESS_FILE` (rewrite it as you go);
- for `ffmpeg` runtimes nothing is needed: the agent reads ffmpeg's own `Duration:` and `time=` output (or
  `-progress pipe:1`). Agents choose this per runtime with `progress_parsers` in `agent.yaml` (`auto`,
  `ffmpeg` or `none`).

A task that says nothing shows `running` without a percentage. Each task has a `phase` and a `percent`
(0-100 over the whole attempt: downloading is 0-5, running 5-95, uploading 95-100) and, while it runs,
`progress` with the latest report. A job's `percent` counts finished tasks as 100 and running tasks by their
own percent; it reaches 100 only when the job has finished. `umm submit -wait`, `umm watch`, the SDK's
`WaitJob`/`WatchJob` and the dashboard follow this live.

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

// Big files go up in parallel 8 MiB parts (4 at a time), each retried on its own.
err := c.UploadFile(ctx, "myapp/part-1.mov", "part-1.mov", &client.TransferOptions{
    OnProgress: func(p client.Progress) { log.Println(p) }, // "45% 120.0/266.0 MiB 38.2 MiB/s"
})

job, err := c.SubmitJob(ctx, api.JobSpec{Name: "encode", Tasks: []api.TaskSpec{{
    Key: "part-1", Runtime: "ffmpeg",
    Args:      []string{"-i", "{in:src}", "-c:v", "hevc_videotoolbox", "{out:enc.mp4}"},
    Inputs:    []api.Input{{Name: "src", Blob: "myapp/part-1.mov"}},
    Outputs:   []api.Output{{Name: "enc.mp4"}},
    Resources: api.Resources{RAMMB: 2048, GPU: true},
}}})

// Follows the job's live stream (polls every 3 s where streaming is not possible).
final, err := c.WaitJob(ctx, job.ID, 3*time.Second, func(j *api.Job) { log.Printf("%s %.0f%%", j.Phase, j.Percent) })
_, err = c.DownloadOutputFile(ctx, final.Tasks[0].ID, "enc.mp4", "enc.mp4", nil)
```

`TransferOptions` sets `PartSize` (default 8 MiB, at least 5 MiB), `Concurrency` (default 4), `Retries` per
part, `Direct` (send parts straight to S3 with presigned URLs instead of through the router) and
`OnUpload`, which hands you the upload ID so that `ResumeUpload` can finish an interrupted upload later.
`UploadBlob` and `DownloadOutput` still stream a file in one request.

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

### Huge single files

A task needs room on one Mac for its inputs and outputs: by default the router assumes twice the input size
(set `resources.disk_estimate_mb` to override) and only places it on a Mac with that much work space. A
task that no Mac could ever hold fails at once with "no Mac has enough work space". A single 500 GB video
will usually not fit on one Mac mini (a 512 GB disk keeps at least 15 % or 50 GiB for its user), and one Mac
would take a long time anyway. Split it by time instead:

1. **Split where the file is**, without re-encoding (stream copy runs at disk speed):

   ```sh
   ffmpeg -i big.mov -map 0 -c copy -f segment -segment_time 600 -reset_timestamps 1 seg%04d.mov
   ```

   Upload the segments (`umm upload`) and submit one map task per segment, as above. Cuts land on
   keyframes, so segments are close to, not exactly, 600 s.
2. **Or let each map task read only its own time range** from storage, so nothing is pre-split and each Mac
   downloads only that part. ffmpeg seeks over HTTP with `Range` requests (MP4/MOV need the index at the
   front: `-movflags +faststart`; MKV and MPEG-TS work as they are):

   ```text
   ffmpeg -ss 3600 -t 600 -i "$SRC_URL" -c:v hevc_videotoolbox ... {out:part.mov}
   ```

   Give the URL to the task in `env` (e.g. `SRC_URL`, a presigned URL valid for the job's duration) and
   allowlist a small wrapper script that calls ffmpeg with it, rather than putting the URL in `args`
   where other users of the Mac could see it in the process list. Do not list it as an `input`, or the agent
   downloads the whole file. With `task_network: deny` the storage host must be in `task_network_allow`.
   Set `disk_estimate_mb` to about twice one part's output, since the router cannot size a URL.
3. **Reduce** by joining the parts with the concat demuxer, also without re-encoding:

   ```text
   ffmpeg -f concat -safe 0 -i parts.txt -c copy {out:final.mov}
   ```

   The reduce task holds all parts and the result (twice the output size), so it needs one Mac with that
   much room; for a very large result, have your app concatenate from storage instead.

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
- **Preflight.** At startup, every minute (`preflight_interval`) and on `SIGHUP` (which re-reads the
  `runtimes` block; other settings need a restart) the agent checks that each runtime's program exists, is a
  regular file and is executable by `_umm`. Runtimes that fail are not offered: the dashboard shows them
  struck through on the **Macs** page (hover for the reason), `umm macs` lists them under *Unavailable
  runtimes*, and the scheduler never places a task needing one there. Install the program and the runtime
  comes back within a minute, without a restart.
- **Exit 127.** If a task still cannot start its runtime on a Mac (exit 127 "command not found", or the
  program is missing or not executable at start), that is the Mac's fault, not the task's: the attempt ends as
  `unavailable`, does not use up `max_retries`, and the task is rescheduled on another Mac at once. The runtime
  stays unavailable on that Mac until a later preflight sees the program changed (or 15 minutes pass), and a
  `mac.runtime_unavailable` event is logged. After `max_mac_faults` (5) such attempts a task's further ones
  count as normal failures, so it cannot bounce forever.
- Tasks run as the hidden non-admin `_umm` user at background priority (`taskpolicy -b`), in a fresh private
  work dir that is deleted afterwards. Install the tools they need where that user can read them.

### Isolation settings

```yaml
work_volume:            # hidden APFS volume mounted at work_dir (macOS; created by the root side)
  enabled: true
  name: umm-work
  quota: auto           # auto = disk size minus min_free_disk, or a fixed size such as 400g
min_free_disk:          # kept free for the Mac's user: the larger of the two
  min: 50GiB
  percent: 15
task_network: allow     # deny: _umm may only reach the router and task_network_allow (pf)
task_network_allow: []  # e.g. [s3.example.com, 198.51.100.0/24]
home_check:
  root: /Users          # the Mac refuses work while a home here is readable by _umm
  ignore: [Shared]
```

The Mac is shown **unhealthy** (with the reason) and gets no work while a user's home folder is open to
`_umm`, free disk is below `min_free_disk`, the work volume is not mounted, or `task_network: deny` is not
in force. The root updater applies these within one update cycle (15 minutes), including on Macs enrolled
before; `sudo umm-agent prepare` applies them at once. See [design.md](design.md#task-isolation).

## Storage

Inputs and outputs never go through the agent's connection to the router. Agents get short-lived URLs.

| Option | Use when | How |
| --- | --- | --- |
| Router storage, `local` backend (default) | small to medium files, no extra infrastructure | upload with `PUT /v1/blobs/KEY` (or `file:` in the CLI), refer to it as `blob: KEY`. Outputs are kept at `jobs/<job>/<task-key>/<output>` |
| Router storage, `s3` backend | big files, many Macs | same API; the router hands agents presigned URLs for any S3-compatible store |
| Your own URLs | the data already lives somewhere | `inputs[].url` (GET) and `outputs[].url` (PUT), e.g. presigned URLs from your own store |
| Another task's output | map/reduce | `from: {task, output}` |

Blob keys are relative paths: no leading `/`, no `..`. Outputs are overwritten per key, so re-runs are safe.

### Large files

Files bigger than one part (8 MiB by default) move in parallel parts, both ways and with both backends:

- **Inputs** (`umm upload`, `umm submit`, the SDK's `UploadFile`): `POST /v1/uploads` with `{key, size,
  part_size}` returns a URL per part. `PUT` each part (any order, several at once), then `POST
  /v1/uploads/{id}/complete`. A failed part is simply sent again. `GET /v1/uploads/{id}` lists the parts
  already stored and fresh URLs for the rest, which is how an interrupted upload resumes (`umm upload FILE
  -resume ID`). Part URLs starting with `/` are on the router and need your `Authorization` header; with
  `"direct": true` and the `s3` backend they are presigned S3 URLs that must be sent without it.
- **Outputs**: agents upload outputs that go to router storage in parallel parts, to presigned (S3) or
  signed (local) URLs. Outputs with your own `url` get a single `PUT`.
- **Downloads**: agents and `umm download` fetch in parallel byte ranges when the server supports `Range`
  (the router and S3 do), and stream otherwise.

Agents set the part size and parallelism in `agent.yaml` (`transfer: {part_size_mb: 8, concurrency: 4}`).
Uploads that are never completed are aborted, and their parts deleted, after 24 hours. A part can be up to
5 GiB and an upload up to 10,000 parts; the router's `max_upload_mb` still limits the whole file.

## The dashboard

The router serves the dashboard at its root URL. It works on a phone: the sidebar becomes a menu, tables
become lists and buttons are big enough to tap. Light and dark mode follow the switch at the top right.
Viewers see everything read-only; only admins get the buttons that change or delete things.

- **Overview** (the home page): tiles for Macs online, idle, busy and yielded to their user, tasks running and
  queued, the success rate of the last 24 hours and the compute harvested (task time on Macs, 24 hours and 7
  days). Below them, charts of fleet utilisation, throughput (succeeded and failed attempts), queue depth and
  yields over 1 hour to 30 days, a live card per Mac (CPU of our tasks and of the Mac's user over the last
  hour, GPU, memory, who is at the Mac, thermal state, power, agent version and update state), the latest
  jobs and the latest events. It refreshes every few seconds while the tab is visible; **Live** pauses it.
  Every chart has a **Table** view with the same numbers.
- **Macs**: the fleet with live telemetry. Open a Mac for its charts (CPU split between our tasks and its
  user, GPU and memory, tasks running, availability), its task history, events and update history. Admins
  can rename, drain, opt out or delete it (delete asks first; deleting revokes the Mac's credential and removes its
  history). Install links are listed below the Macs; used, expired and revoked ones can be deleted one by one
  or with **Clear old**.
- **Jobs**: filter by state; open a job for its timeline (every attempt of every task on the Mac that ran it,
  by task or by Mac, coloured by outcome), its tasks, and a task's attempts, logs and placement scores. Admins
  can cancel a job and delete finished jobs (tick several, or **Delete old jobs** for everything finished more
  than N days ago).
- **Activity**: enrolments, yields, quarantines, failed jobs, agent update results and every admin change
  (created, revoked and deleted keys, deleted Macs, jobs and install links, retention changes), with who did
  it.
- **API keys** (admins): create and revoke keys; delete revoked ones (one by one or all at once).
- **Settings**: scheduler weights and limits, agent updates, and **Data and retention**: how long 1-minute
  and 15-minute metrics and the activity log are kept, whether finished jobs are deleted automatically, and
  one-off cleanup buttons.

The CLI has the same cleanup: `umm keys delete ID` / `umm keys purge`, `umm install-tokens delete ID` /
`umm install-tokens purge`, `umm macs delete MAC`, `umm jobs delete JOB` / `umm jobs purge -older-than 30d
[-state failed]`, plus `umm metrics`, `umm events [-kind mac.]` and `umm retention [set -jobs-days 90]`.

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
| `POST /v1/uploads` | app, admin | start a multipart upload `{key, size, part_size?, direct?}` → `{id, parts, part_size, urls}` (`201`) |
| `GET /v1/uploads/{id}` | app, admin | parts already stored (`done`) and fresh URLs for the rest |
| `PUT /v1/uploads/{id}/parts/{n}` | app, admin | store part `n` (raw body, exactly `part_size` bytes except the last) |
| `POST /v1/uploads/{id}/complete` | app, admin | assemble the blob once every part is stored → `{blob, size, parts}` |
| `DELETE /v1/uploads/{id}` | app, admin | abort an upload and drop its parts |
| `GET /v1/jobs/{id}/stream` | any | server-sent events: `job` (the job with its tasks) on every change, then `end`, or `removed` |
| `GET /v1/jobs/stream?state=&limit=` | any | server-sent events: `jobs` (the list), then `job` and `removed` as jobs change |
| `GET /v1/blobs/{key}` | app, admin | download a blob |
| `GET /v1/macs` | any | Macs with state and live telemetry |
| `PATCH /v1/macs/{id}` | admin | `{"drain": true}`, `{"opted_out": true}` or `{"name": "Lab Mac 2"}` |
| `DELETE /v1/macs/{id}` | admin | delete a Mac: revokes its credential and removes its metrics history |
| `POST /v1/enrolment-tokens` | admin | one-time token for a new Mac |
| `GET` / `POST /v1/api-keys`, `DELETE /v1/api-keys/{id}` | admin | list, create (`{"name", "role"}`) and revoke API keys (`purge` below deletes them) |
| `GET /v1/settings` | viewer, admin | scheduler weights and limits |
| `PUT /v1/settings` | admin | change them at runtime |
| `GET /v1/updates` | viewer, admin | agent update policy, router version, relay status |
| `PUT /v1/updates` | admin | change the agent update policy (see [Agent updates](#agent-updates)) |
| `GET /v1/macs/{id}/updates` | viewer, admin | a Mac's update history, newest first |
| `GET /v1/install/profiles` | admin | install profiles configured on the router |
| `POST /v1/install-tokens` | admin | one-line install link: `{profile, labels, ttl_minutes, max_uses, note}` → `{id, token, command, ...}` |
| `GET /v1/install-tokens` | admin | install links, newest first (no secrets) |
| `DELETE /v1/install-tokens/{id}` | admin | revoke an install link (`purge` below deletes it) |
| `GET /v1/install-tokens/{id}/events` | admin | an install link's audit log |
| `GET /v1/access` | admin | dashboard role rules (config rules `locked`), default role, group names |
| `POST /v1/access/rules` | admin | add or change a rule: `{kind: group\|user, value, role: admin\|viewer, name?}` |
| `DELETE /v1/access/rules/{id}` | admin | remove a dashboard rule (`403` for config rules) |
| `PUT /v1/access/default` | admin | `{"role": "deny"\|"viewer"}` for users no rule matches |
| `PUT /v1/access/groups/{id}` | admin | `{"name": "..."}` display name for a group ID (`""` removes it) |
| `GET /v1/access/users` | admin | people who signed in: last sign-in, effective role, reason, deciding rule |
| `PUT /v1/access/users/{subject}/role` | admin | `{"role": "admin"\|"viewer"\|""}` a user's own rule (`""` removes it) |
| `GET /v1/access/audit?limit=` | admin | role-change audit log, newest first |
| `GET /i/{token}`, `GET /i/uninstall` | the token / public | one-line install and uninstall scripts |
| `PUT /v1/updates/artifacts/{version}/{file}` | admin | seed the relay cache (routers without internet) |
| `GET /v1/metrics/summary` | viewer, admin | headline numbers: Macs by state, tasks, success rate and compute hours (24 h, 7 d) |
| `GET /v1/metrics/fleet?range=24h&step=` | viewer, admin | fleet time series (also `from`/`to` as RFC 3339 or Unix ms; `step` in seconds, default about 120 points) |
| `GET /v1/metrics/macs?window=1h` | viewer, admin | a short series per Mac (sparklines) |
| `GET /v1/metrics/macs/{id}?range=7d` | viewer, admin | one Mac's time series |
| `GET /v1/macs/{id}/attempts?limit=` | viewer, admin | a Mac's task history, newest first |
| `GET /v1/jobs/{id}/timeline` | any | every attempt of a job's tasks, with Mac and times |
| `DELETE /v1/jobs/{id}` | admin | delete a finished job with its tasks, logs and stored outputs (`409` while it runs) |
| `POST /v1/jobs/purge` | admin | `{"older_than_days": 30, "states": ["failed"]}` → `{"deleted": n}` |
| `POST /v1/api-keys/{id}/purge` | admin | delete a revoked key (`409` while active) |
| `POST /v1/api-keys/purge` | admin | delete every revoked key |
| `POST /v1/install-tokens/{id}/purge` | admin | delete a used, expired or revoked install link and its audit log |
| `POST /v1/install-tokens/purge` | admin | delete every install link that can no longer enrol |
| `GET /v1/events?kind=mac.,key.&target=&before=&limit=` | viewer, admin | activity and audit log, newest first (a kind ending in `.` matches a prefix) |
| `GET` / `PUT /v1/retention` | viewer / admin | how long metrics, events and finished jobs are kept |
| `GET /v1/docs`, `GET /v1/docs/{slug}` | public | these docs (index, raw Markdown) |
| `GET /healthz` | public | liveness |
| `GET /metrics` | public | Prometheus metrics |

Apps that can see a job can also read its tasks, logs and outputs. Errors come back as
`{"error": "message"}` with a 4xx or 5xx status.

## Enrolling Macs

Requirements: Apple Silicon, a recent macOS, outbound HTTPS to the router, and an admin account on the Mac.

### Add a Mac with one command

In the dashboard open **Macs**, then **Add Mac**, **One command** (admins only). Pick a profile (labels and
runtimes, defined in the router config under `install.profiles`), optional extra labels, how long the link
lives (1 hour by default) and whether it is single use (default) or good for a batch of N Macs. Copy the
command and run it in Terminal on the Mac:

```sh
curl -fsSL https://umm.example.com/i/umm_ins_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX | sudo sh
```

From the CLI, `umm enrol-command [-profile lab] [-label room=lab-2] [-ttl 1h] [-uses 10]` prints the same
command, and `umm install-tokens list|log ID|revoke ID` manages links. REST: `POST /v1/install-tokens`.

The script asks for the Mac admin's password (sudo) and then:

1. checks it is an Apple Silicon Mac on macOS 12 or later;
2. downloads the agent release the router wants (the Settings → Agent updates target, else the router's own
   version) from the router's signed-release cache;
3. verifies the minisign signature of `SHA256SUMS` with the release public key pinned in the script
   (including the version in the trusted comment) and the bundle's SHA-256. Any failure stops it before
   anything is installed. Stock macOS has no minisign, so the check runs in JavaScriptCore through
   `osascript -l JavaScript` (no UI);
4. installs exactly like `install.sh`: hidden non-admin `_umm` user, `/usr/local/bin/umm-agent`,
   `agent.yaml` with the profile's labels and runtimes allowlist, the agent LaunchDaemon and the root updater
   LaunchDaemon, under the profile's service label (set a neutral one such as `com.example.helper` with
   `install.service_label`);
5. enrols: the token is written to a root-only temp file and handed to `umm-agent enrol` over stdin, never
   in argv. Download URLs, which carry the token, reach `curl` through a config on stdin;
6. starts the daemons, waits for the new agent's first heartbeat and prints one success line.

Running it again (with a link that is still valid) upgrades or repairs the agent without enrolling it again
and without using up the link. To remove the agent:

```sh
curl -fsSL https://umm.example.com/i/uninstall | sudo sh                 # keep config and credential
curl -fsSL https://umm.example.com/i/uninstall | sudo sh -s -- --purge   # remove everything
```

Then delete the Mac in the dashboard.

**Link security.** The token is a 256-bit secret, stored only as a SHA-256. A link expires (1 hour by
default, 7 days at most), works for 1 to 1000 enrolments, and can be revoked at once under **Install
links** on the Macs page. Fetching the script does not use it up; a successful enrolment does. An expired,
used or revoked link answers with a script that prints a clear error and exits non-zero. `/i/` allows 30
requests a minute per client address. The router serves it only when `public_url` is https
(`install.allow_http: true` is for development). Every script fetch, bundle download, enrolment, refusal
and revocation is recorded with its time and client address (**Install links → Audit log**). The token is
part of the URL, so it can end up in proxy access logs and the admin's shell history: keep links
short-lived.

**Quiet.** Nothing is shown on the Mac beyond the Terminal output. macOS 13+ still posts a "Background Items
Added" notification for new LaunchDaemons unless an MDM profile marks them as managed; see
[Fleet install: Quiet install](fleet.md#quiet-install).

### Other ways

**One Mac by hand.** Download `umm-agent-darwin-arm64.tar.gz` from the release, copy it to the Mac, unpack
it and run:

```sh
sudo UMM_SERVER=https://umm.example.com sh install.sh
```

Paste the one-time token when asked. Get it from the dashboard (**Macs**, **Add Mac**, **Manual**) or with
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
- Admins can **rename** a Mac from its actions menu in the dashboard or with `umm rename MAC NAME`. A Mac
  enrols with its ComputerName; after that the router's name is the one that counts (the agent never sends
  it again, so a rename sticks and `name:` in the Mac's `agent.yaml` is not changed). Names are 1-64
  characters from `A-Z a-z 0-9`, space, `.`, `_` and `-`, the same rule as the installer. They need not be
  unique: Macs are identified by ID.
- `umm-agent telemetry` on a Mac prints what the agent sees: console user, idle time, load, memory, thermal,
  power.
- Deleting a Mac in the dashboard revokes its credential. Re-enrol it with a new token.

## Agent updates

Macs keep their agent up to date by themselves. A small root updater on each Mac (`umm-agent update`, its
own LaunchDaemon) checks in with the router at boot and every 15 minutes. The router says which version the
Mac should run; the updater installs it only if the release is **signed** with the project's release key.
Running tasks are allowed to finish first, and if the new agent does not report in within two minutes the
updater puts the previous one back. Nothing is shown on the Mac.

**Which version.** By default every Mac runs the router's own version, so upgrading the router upgrades the
fleet. In the dashboard (**Settings**, **Agent updates**) or with `PUT /v1/updates` an admin can:

| Setting | Does |
| --- | --- |
| `paused` | stop all agent updates |
| `target_version` | pin the fleet to a version, e.g. `v0.1.3`. Pinning is also the only way to go **back** to an older version |
| `canary_version` | a version for the canary Macs only; the rest stay on the target |
| `canary_percent`, `canary_macs` | who the canaries are: a stable share of the fleet and/or listed Mac IDs |
| `drain_timeout_seconds` | how long to wait for running tasks (default 1 h); after that the agent restarts anyway and its tasks are requeued, not failed |

```sh
curl -X PUT -H "Authorization: Bearer $UMM_API_KEY" -H 'Content-Type: application/json' \
  https://umm.example.com/v1/updates \
  -d '{"canary_version":"v0.1.3","canary_percent":10,"canary_macs":[],"target_version":"","paused":false,"drain_timeout_seconds":0}'
```

A typical rollout: set the canary version, watch the canaries' **Version** column on the **Macs** page, then
set the target to the same version (or clear both after upgrading the router).

**Watching it.** The **Macs** page shows each agent's version, the version the router wants (`→ v0.1.3`) and
the updater's last state (`downloading`, `draining`, `applying`, `succeeded`, `rolled back`, `refused`,
`failed`); hover for details, click the version for the history. "no updater" means the Mac's updater has
never checked in (see [Fleet install](fleet.md#agent-self-update) to add it). On the Mac:
`/usr/local/var/log/umm-updater.log`, and `sudo umm-agent update -dry-run` checks and verifies without
installing.

**Where downloads come from.** By default the router fetches releases from GitHub once and serves them to
the Macs (`/v1/agent/update/artifacts/...`), so Macs need no internet access. Router settings in
`server.yaml`:

```yaml
updates:
  relay: true          # false: Macs download from release_url themselves
  release_url: https://github.com/alee-ibrahim/use-my-mac/releases/download
  offline: false       # true: the router never fetches; upload the files yourself
```

For an offline router, upload `SHA256SUMS`, `SHA256SUMS.minisig` and `umm-agent-darwin-arm64.tar.gz` of a
release with `PUT /v1/updates/artifacts/<version>/<file>` (admin key, raw body). The relay is only a cache:
the Macs check the signature themselves.

**On one Mac.** `update.enabled: false` in `/usr/local/etc/umm/agent.yaml` stops that Mac from updating;
`update.source` picks `auto` (relay, else direct), `router` or `direct`.

## Troubleshooting

| Symptom | Look at | Likely cause |
| --- | --- | --- |
| Task stays `pending` | the task's **Placement** tab in the dashboard: every Mac and why it was skipped | no Mac has the runtime, not enough RAM or disk, labels don't match, all Macs busy, yielding, drained or offline |
| Task keeps moving between Macs | attempts with outcome `yielded` | the Macs' users are busy. Normal; it runs when a Mac is quiet. Long tasks prefer idle Macs if you set `expected_seconds` |
| Task shows `running` with no percentage | the task's output | the task reports nothing: print `UMM_PROGRESS=N` or write `$UMM_PROGRESS_FILE` (see [Progress](#progress)) |
| `umm watch` or the dashboard updates only every few seconds | proxies between you and the router | the proxy buffers server-sent events, so clients fall back to polling. Turn off buffering for the `/v1/jobs/` streams |
| Task `failed` | `umm logs TASK` (all attempts) | the program exited non-zero, timed out, or did not write a declared output to `out/NAME` |
| `401 Unauthorized` | the key | missing, mistyped or revoked key; the header must be `Authorization: Bearer umm_key_...` |
| Signed in but sent back to sign-in, or "not allowed" | **Settings > Access > Users** (an admin) | no rule gives you a role and the default is `deny`, or a rule was just removed; roles apply on the next request |
| `409` changing a role | the error text | the change would remove your own admin role with no other admin left |
| `403` | the error text names your role | an app key used for an admin call, or a viewer trying to change something |
| `400` on submit | the error text | unknown field, duplicate task key, cycle, an input with no or several sources, or a disallowed `env` name |
| Mac shows offline | `umm-agent telemetry` and the agent log on the Mac | network or proxy to the router, a revoked credential, or the daemon is not loaded |
| Mac never `ready` | the Mac's state and reason in the dashboard | paused (`paused` file), drained, quarantined after repeated failures, on battery, or its user is busy |
| Program not found or permission denied; a runtime is struck through on the **Macs** page or listed under *Unavailable runtimes* in `umm macs` | the reason in its tooltip / `umm macs`, and the runtime path in `agent.yaml` | the program is not installed on that Mac (e.g. Homebrew `ffmpeg` missing), or the path or a parent directory is not readable and executable by `_umm`. Install or fix it; the agent re-checks every minute (or run `sudo launchctl kill HUP system/io.github.alee-ibrahim.umm-agent` after editing `agent.yaml`). No task is placed there meanwhile |
| Attempts with outcome `unavailable` (exit 127) | `umm logs TASK`, the **Activity** page (`mac.runtime_unavailable`) | the Mac could not execute the runtime. The task moved to another Mac without using a retry, and that Mac lost the runtime until its program is fixed |
| Mac stays on an old version | its update state and history (click the version on the **Macs** page) | updates paused, a downgrade without a pinned version (`refused`), a version that rolled back (retried after 1 h, then 2 h, ... up to a day), or "no updater" on Macs installed before self-update |
