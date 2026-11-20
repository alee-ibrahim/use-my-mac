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
| `app` | submit jobs, upload inputs, see, cancel and delete **its own** jobs and stored files |
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
| `umm download TASK OUTPUT [-o FILE] [-concurrency 4] [-direct \| -relay]` | download an output (parallel byte ranges, with progress, SHA-256 checked). Straight from S3 when the router uses the `s3` backend; `-relay` streams through the router instead |
| `umm download TASK OUTPUT -url [-ttl 1h]` | print a presigned URL for the output, for other tools (`s3` backend) |
| `umm cancel JOB` | cancel a job |
| `umm jobs rm JOB... [-inputs]` | delete your jobs; running ones are cancelled first. `-inputs` also deletes the input files you uploaded for them |
| `umm jobs rm -finished [-state failed] [-older-than 7d] [-inputs]` | delete all your finished jobs (admins: everyone's, or `-owner`) |
| `umm files [-kind upload\|output] [-job JOB] [-prefix P]` | your stored files: uploads and your jobs' outputs, with size and whether a job still needs them |
| `umm files rm KEY...` / `umm files rm -all [filters]` | delete stored files; files an unfinished job needs are kept |
| `umm macs label MAC k=v... [-rm K]` | router-side labels (admin), see [Choosing Macs](#choosing-macs) |
| `umm macs reserve MAC -for KEY_ID[,...] [-hours N] [-note X]` / `unreserve MAC` | reserve a Mac for some API keys (admin) |
| `umm macs max-tasks MAC N` | how many tasks a Mac runs at once (admin; 0 = its own setting) |
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
| `runtime` | an allowlisted runtime name, a command of a managed runtime (see [Runtimes](#runtimes)), or `mise` for a toolchain the task brings (see [Toolchains with mise](#toolchains-with-mise)) |
| `requires` | managed runtime packages to install before the task runs; their `bin` directories go first on `PATH` (see [Managed runtimes](#managed-runtimes)) |
| `args` | appended to the runtime's fixed argv. Placeholders: `{in:NAME}` input path, `{out:NAME}` where output NAME must be written, `{workdir}` the task's private directory |
| `env` | extra environment variables. `PATH`, `HOME`, `TMPDIR`, `UMM_*`, `DYLD_*` and `LD_*` are not allowed |
| `inputs` | files placed in `in/` before the task starts. Each has a `name` and exactly one of `url`, `blob` or `from` (the CLI also accepts `file`) |
| `outputs` | files the task must write to `out/NAME`. `optional: true` allows a missing file; `url` sends it straight to your own PUT URL |
| `resources` | `ram_mb`, `gpu`, `expected_seconds`, `disk_mb` (scratch space), `disk_estimate_mb` (total work space: inputs, outputs and scratch; default twice the inputs plus `disk_mb`), `labels` (all must match the Mac's labels: its agent's own and those an admin set on the router) |
| `macs` | run only on these Macs (IDs or names). See [Choosing Macs](#choosing-macs) |
| `prefer` | soft placement: `{labels: {...}, macs: [...], bonus: 25}`. Matching Macs get a score bonus; the others still take work |
| `timeout_seconds` | limit for one attempt (router default if 0) |
| `max_retries` | failed attempts retried before giving up (router default if unset). Yields and lost Macs do not count |
| `depends_on` | task keys that must succeed first |

A task's standard output and error are its logs. Small results can simply be printed.

The job itself can also set `macs` and `prefer` (the default for tasks that set none) and `cleanup` (see
[Cleaning up](#cleaning-up)).

### Submitting safely more than once

A feeder that crashes, or times out waiting for the router's answer, cannot know whether its job went in.
Give the job a `client_id` (or send an `Idempotency-Key` header; `umm submit -client-id ID`): it is unique per
owner, and submitting the same spec again with it returns the existing job (`200`, `"existing": true`)
instead of a second one (`201`). The same `client_id` with a different spec is refused (`409`). Tasks can
have their own `client_id`, unique per owner across jobs: a task whose `client_id` already exists is left out
and listed under `existing_tasks` (task, job and state), so a re-sent batch only adds what is missing. If
every task exists, nothing is created and the response is the job of the first one. A new task cannot depend
on one left out that way. Deleting a job frees its client IDs.

### Output checksums

The Mac computes each output's SHA-256 while it uploads it. A task's `output_sha256` (and `sha256` in
`GET /v1/files`) has it; `umm download` checks the downloaded file against it (`-no-verify` skips that) and
the SDK's `VerifyOutputFile` does the same (direct downloads check it themselves). Outputs made by agents from before this have none.

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

With the `s3` backend, `DownloadOutputFile` with `Direct: true` fetches the output straight from the bucket
in parallel ranges on a presigned URL, checks the Mac's SHA-256 (`SkipVerify` skips that) and then tells the
router (`ConfirmDownload`), which is what `after_download` cleanup counts. `DownloadOutputFileInfo` does the
same and says whether it went direct, was verified and was confirmed. `URLTTL` sets the URL lifetime.
`Me` reports the router's `storage` backend, so you can choose. For other tools, `OutputURL` and `BlobURL`
return a presigned URL (`{url, expires_at, size, sha256}`); downloads through it are invisible to the router,
so call `ConfirmDownload(ctx, taskID, name, sha256)` afterwards when the job uses `after_download`:

```go
d, err := c.OutputURL(ctx, taskID, "enc.mp4", time.Hour) // curl -o enc.mp4 "$URL", ffmpeg -i "$URL", ...
// ... fetch d.URL elsewhere, check d.SHA256 ...
err = c.ConfirmDownload(ctx, taskID, "enc.mp4", d.SHA256)
```

The client also has `Job`, `Jobs`, `CancelJob`, `RemoveJob`, `PurgeJobs`, `Files`, `DeleteFile`,
`DeleteFiles`, `Task`, `Logs`, `Macs` and the admin calls (`PatchMac`, `SetMacLabels`, `ReserveMac`,
`UnreserveMac`, `SetMacMaxTasks`, `CreateEnrolmentToken`, `CreateAPIKey`, `APIKeys`, `RevokeAPIKey`).
`examples/video-transcode/` in the repository is a complete map/reduce program.

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

## Choosing Macs

By default the scheduler picks the best Mac for each task (see [Design](design.md#scheduling)). Four things
narrow or steer that:

- **Labels.** `resources.labels` must all match a Mac's labels. A Mac's labels are those in its `agent.yaml`
  plus **router labels** an admin sets on the dashboard's Mac page (**Placement**, **Edit**), with `umm macs
  label MAC class=studio` or `PUT /v1/macs/{id}/labels`. Router labels win when both set the same key, and
  apply at once, with no change on the Mac.
- **`macs`** on a task or job pins it to those Macs (IDs or names). Unknown names are refused at submit.
- **Reservations.** An admin can reserve a Mac for one or more owners (API keys, or `user:<subject>` for
  single sign-on users), optionally until a time: `umm macs reserve studio-1 -for key_abc,key_def -hours 48
  -note "bulk encode"`, the Mac page, or `PUT /v1/macs/{id}/reservation`. A reserved Mac only runs those
  owners' tasks. Their tasks still go anywhere else too; to keep them on the reserved Macs, pin them with a
  label (e.g. a router label `pool=encode` on those Macs) or `macs`. Only the owners can pin to a reserved
  Mac (`403` for anyone else). Apps see a reserved Mac with `reservation.mine` set when it is theirs; only
  admins and viewers see who it is reserved for. A task that only reserved Macs could run waits, and its
  `waiting` field (the dashboard shows it on the task) says so: `only Macs reserved for other owners could
  run it now: studio-1`.
- **`prefer`** is soft: `prefer: {labels: {class: studio}}` adds `bonus` (default 25, on the 0-100 score) to
  matching Macs, so they win when they are free, while the other Macs still take work. Nobody has to drain
  anything.

```yaml
name: encode-batch
prefer: {labels: {class: studio}}      # every task: studio Macs first
tasks:
  - key: part-1
    runtime: ffmpeg
    macs: [studio-1, studio-2]         # this one only there (must be allowed to use them)
    ...
```

**Tasks at once.** A Mac runs `max_tasks` tasks at once (`agent.yaml`, default 1). An admin can change it from
the router: the Mac page, `umm macs max-tasks studio-1 4` or `PATCH /v1/macs/{id}` `{"max_tasks": 4}` (0
goes back to the Mac's own value). The agent applies it on its next heartbeat, capped at `max_tasks_limit`
in its `agent.yaml` (default 8), so the Mac's owner keeps the final say. Lowering it never stops running
tasks. Agents from before this setting keep their own `max_tasks`.

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
### Managed runtimes

Instead of installing a tool on every Mac by hand, an admin can publish it once as a **managed runtime**: a
signed package each Mac verifies and installs the first time a task needs it: no Homebrew, no sudo, no
SSH. The agent only asks; a small root helper on the Mac installs it read-only into
`/usr/local/var/umm-runtimes`, so tasks cannot change it, and the agent re-checks the files before every
use (see [design.md](design.md#managed-runtimes)).

1. Build the tool for each platform as a `tar.gz`, `zip` or single executable and upload it to the router
   (or host it anywhere the Macs can reach):

   ```sh
   umm runtimes upload ffmpeg-7.1-darwin-arm64.tar.gz runtimes/ffmpeg-7.1-darwin-arm64.tar.gz
   # prints "blob": ..., "sha256": ..., "size": ... for the manifest
   ```

2. Write the manifest (`ffmpeg.json`). Commands are paths inside the package; tasks can only append
   arguments:

   ```json
   {
     "name": "ffmpeg",
     "version": "7.1",
     "description": "FFmpeg static build",
     "commands": {"ffmpeg": ["bin/ffmpeg"], "ffprobe": ["bin/ffprobe"]},
     "artifacts": [
       {"os": "darwin", "arch": "arm64", "format": "tar.gz",
        "blob": "runtimes/ffmpeg-7.1-darwin-arm64.tar.gz", "sha256": "…", "size": 41234567}
     ]
   }
   ```

3. Sign it and publish it. The secret key comes from a file or `UMM_RUNTIME_SIGNING_KEY`, never argv:

   ```sh
   umm runtimes keygen -out runtimes.key      # once; prints the public key for agent.yaml
   umm runtimes sign ffmpeg.json -key-file runtimes.key
   umm runtimes publish ffmpeg.json [-prewarm]
   ```

- **Trust.** The router only relays. A Mac installs a package only if its signature verifies with a key in
  its root-owned `agent.yaml` (`managed_runtimes.public_keys`, by default the release key that already
  decides which agent binary runs) and `managed_runtimes.allow` lists its name (default `*`). The signature
  covers the name, version and the SHA-256 of every download, so where the bytes come from does not matter.
  Archives are unpacked with strict checks (no absolute paths, `..`, links out of the package, hard links,
  devices or setuid bits), owned by root and read-only.
- **Placement.** Tasks that name a package command (or list a package in `requires`) can go to Macs that
  have it or can install it; Macs that already have it are preferred (score × 0.7 otherwise) and the
  placement view shows *install: ffmpeg*. On a Mac that does not have it yet the first attempt ends
  `unavailable` ("install requested") without using up `max_retries`; the Mac installs it within seconds
  and gets the task back once it is ready, unless another Mac that has it takes it first. A failed install
  marks the runtime unavailable on that Mac (retried after `retry_after`, 10 minutes). If a package's files
  no longer match what root installed, the task is refused there and the package is reinstalled.
- **Pre-install.** `umm runtimes prewarm ffmpeg` (or the switch on the dashboard **Runtimes** page) makes
  every Mac that can install it do so in the background.
- **Updates.** Publishing a new version replaces the old one. Macs install it on next use (or at once if
  pre-installed) and delete old versions once no running task uses them. `umm runtimes rm NAME` withdraws a
  package.
- **Status.** `umm runtimes`, `umm runtimes show NAME` and the dashboard **Runtimes** page show each package
  per Mac: installed, installing, outdated, installable or failed (with the error).
- Turn it off per Mac with `managed_runtimes: {enabled: false}` in `agent.yaml`; the static `runtimes`
  allowlist works as before and wins when both define a command. The helper logs to
  `/usr/local/var/log/umm-runtimes.log`; `sudo umm-agent runtimes-sync` runs a pass by hand.

### Toolchains with mise

A job that needs several tools at exact versions (say a Node.js script that calls ffmpeg) can bring them
itself, as a [mise](https://mise.jdx.dev) config, instead of an admin installing each tool on every Mac.
The task names the tools and versions; the Mac's own mise installs them for that task and runs the task's
program with them. This is **off by default**. The admin turns it on per Mac in `agent.yaml`:

```yaml
mise:
  enabled: true
  binary: /usr/local/bin/mise    # installed and verified by the admin; tested with mise 2026.11.9
  # commands: [node, ffmpeg]     # optional: the only programs tasks may run (default: any program a tool provides)
  # require_lockfile: true       # default: installs are locked to mise.lock's URLs and checksums
  # cache_dir: /usr/local/var/umm/mise-cache   # shared download cache
  # max_cache: 2GiB              # least recently used downloads go first
  # Optional, for tools released from private GitHub repositories (at most one of the two):
  # github_token_file: /usr/local/etc/umm/github-token          # mode 0600, owned by _umm
  # github_token_command: ["/usr/local/bin/your-secrets-cli", "kv", "get", "-field=token", "secret/github-read"]
```

The Mac then offers the runtime `mise`, so only Macs that enabled it get such tasks. Instead of `binary`,
the program can come from a `runtimes: {mise: [...]}` entry or a [managed runtime](#managed-runtimes) with a
`mise` command, so a signed mise reaches every Mac without hand installs. Install mise from its GitHub
release and check it against the release's checksums (or publish it as a managed runtime); the agent runs
whatever `binary` points at.

A task sends `mise.toml` and `mise.lock` as inputs and names a program in its first argument:

```toml
# mise.toml
[tools]
node = "24.22.3"
ffmpeg = "8.0"
```

```sh
mise lock --platform macos-arm64   # writes mise.lock with each download's URL and checksum
```

```yaml
name: report
tasks:
  - key: render
    runtime: mise
    args: ["node", "{in:render.js}", "{out:report.html}"]
    inputs:
      - {name: mise.toml, file: ./mise.toml}
      - {name: mise.lock, file: ./mise.lock}
      - {name: render.js, file: ./render.js}
    outputs: [{name: report.html}]
    resources: {disk_estimate_mb: 2048}
```

`examples/mise-node/` in the repository is a complete job.

How it runs, on the Mac, in the task's own work dir:

1. The agent copies `mise.toml` and `mise.lock` into the work dir and asks mise what they contain
   (`mise config explain --json`, `mise ls --current --json`). Only `[tools]` and `min_version` are
   accepted: `[env]`, `[tasks]`, `[hooks]`, `[settings]` and the rest fail the task. mise reads the config
   in safe mode (`MISE_SAFE=1`), so nothing in it runs code: tool options such as `postinstall` or
   `install_env` make the install fail. Global, system and parent-directory mise config is ignored.
2. `mise install` installs the tools into the work dir (its output is in the task log). With
   `require_lockfile` it runs locked: every tool must be in `mise.lock` with a URL and checksum for the
   Mac's platform (`macos-arm64`), and mise checks each download against it.
3. The program must be one the installed tools provide (`mise which`), never a system program such as
   `/bin/sh`, and in `commands` when that is set. The agent runs it with `mise exec --`, at the task's
   priority, as `_umm`, like any other task.

What to know:

- **Any backend.** Tools can come from any mise backend (the registry, `github:`, `aqua:`, `ubi:`, `npm:`,
  `pipx:`, `cargo:`, `go:` and so on). Some backends build or run package installers (npm, pip, cargo, go),
  which runs code from the package while installing. Tools installed through `pipx:` need `uv` (or
  `pipx`) as a pinned tool in the same `mise.toml`.
- **The work dir holds everything.** Tools are installed per task and deleted with the work dir, so count
  them in `resources.disk_estimate_mb` (Node.js is about 200 MB unpacked). Downloads a tool makes by itself
  later (uv's own Pythons, a browser a test framework fetches) go under the task's `HOME`, which is its work
  dir, too. Only mise's download cache is shared between tasks: with `require_lockfile` mise keeps only
  downloads with a locked checksum and checks them on every install, so a task cannot plant a file for the
  next one. It is capped by `max_cache`; without a lockfile nothing is shared.
- **Network.** Installs download from wherever the tools are published (e.g. `nodejs.org`, `github.com`
  and `release-assets.githubusercontent.com`). With `task_network: deny`, list those hosts in
  `task_network_allow`, since the agent installs as `_umm`. mise's GitHub-based backends (`aqua:`,
  `github:`, `ubi:`) also call `api.github.com`, even for a locked install. GitHub allows only 60
  unauthenticated API calls an hour per public IP address, shared by every Mac behind it, so a fleet
  behind one address should set `github_token_file` or `github_token_command`: for public releases a
  token with no scopes (or a fine-grained token with no repository access) is enough. A download in the shared cache is used without
  fetching it again, though mise may still ask the host whether it changed.
- **Timeout.** `timeout_seconds` covers installing and running.
- **Errors.** A config the Mac refuses, a tool mise cannot install (a wrong checksum, a platform missing
  from the lockfile, a prebuilt binary that needs a library the Mac lacks), a program the tools do not
  provide and `MISE_*` or `GITHUB_TOKEN` variables in the task's `env` fail the task with the reason; mise's
  own output is in the task log. Running out of disk while installing is a Mac fault, as is a missing mise
  binary (the runtime is then unavailable on that Mac).
- **GitHub tokens.** `github_token_file` or `github_token_command` give mise a GitHub token, for the API
  rate limit above and for releases in private repositories. An invalid or expired token fails installs
  that call the GitHub API ("Bad credentials" in the task log). The agent reads the file, or runs the command (30 s limit; its standard output is
  the token, its standard error is discarded), before each install, and passes the token to `mise install`
  only, as `MISE_GITHUB_TOKEN`: never to the task's program and never to the log. Tasks run as the same
  user as the agent, though, so a task could read the token file or run the command itself: use a
  fine-grained token that can only read the repositories you need, and enable it only on Macs that run jobs
  you trust with it. If only one job needs a private program, ship the program as a task input instead.
  Never put a token in a task's `env`: it is stored with the job (the agent refuses `GITHUB_TOKEN` there).

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
  (the router's `local` backend and S3 do), and stream otherwise.

### Direct downloads (s3 backend)

With the `s3` backend the bytes of an output or blob need not pass through the router.
`GET /v1/tasks/{id}/outputs/{name}?direct=1` and `GET /v1/blobs/{key}?direct=1` return a presigned `GET` URL
instead of the file:

```json
{"url": "https://s3.example.com/bucket/...&X-Amz-Signature=...", "expires_at": "2026-01-01T13:00:00Z",
 "size": 12582912, "sha256": "9f86d0..."}
```

- Fetch the URL with a plain `GET`, without your `Authorization` header. `Range` headers work (parallel
  ranges); the URL signs only the host, so send no other headers such as `Content-Type` or `x-amz-*`.
- `ttl=SECONDS` (60 to 604800, 7 days) sets the lifetime; the default is the router's
  `storage.direct_download_ttl_minutes` (60). `redirect=1` answers `307` to the URL instead of JSON, for tools
  that follow redirects.
- Who may ask is the same as for streaming: the job's owner or an admin (viewers get `403`), and a blob's owner
  or an admin (`404` for other app keys).
- With the `local` backend `direct=1` gets `400` ("direct downloads need the s3 storage backend"); download
  without it. `GET /v1/me` reports the backend as `storage`, and `umm download` goes direct only when it is
  `s3`.
- The router never sees these bytes. For `after_download` cleanup, confirm a complete download with
  `POST /v1/tasks/{id}/outputs/{name}/downloaded` and `{"sha256": "<hex>"}` (optional; a mismatch with the
  Mac's checksum is refused with `409` and not counted). `umm download` and the SDK do that after the checksum
  passes. Issuing a URL alone never counts.

Agents set the part size and parallelism in `agent.yaml` (`transfer: {part_size_mb: 8, concurrency: 4}`).
Uploads that are never completed are aborted, and their parts deleted, after 24 hours. A part can be up to
5 GiB and an upload up to 10,000 parts; the router's `max_upload_mb` still limits the whole file.

### Who owns what

Every file the router stores has an owner: an upload belongs to the key (or user) that uploaded it, a task
output to the job's owner. Only the owner and admins can read, overwrite or use an upload as a job input;
another app key gets `404` reading it and `403` writing or using it. Files stored before the router tracked
owners have none recorded and keep working as before (any app key can use them; only admins can delete them).

### Cleaning up

Bulk runs leave inputs and outputs on the router until someone deletes them. Owners (app keys) and admins
can:

- **Delete a job** (`umm jobs rm JOB`, `DELETE /v1/jobs/{id}`, the SDK's `RemoveJob`, or the dashboard): a
  queued or running job is cancelled first (its Macs stop the tasks), then the job, its tasks, logs and
  stored outputs are deleted. With `-inputs` (`?inputs=true`) the input files its owner uploaded go too,
  unless another unfinished job uses them. `umm jobs rm -finished [-state failed] [-older-than 7d]` (`POST
  /v1/jobs/purge`) deletes many at once; app keys only ever touch their own jobs.
- **Delete stored files** (`umm files`, `GET /v1/files`, `DELETE /v1/files/{key}`, `POST /v1/files/delete`):
  list your uploads and outputs with their size, then delete them by key (`uploads/...`, `jobs/...`, or the
  blob key you uploaded) or all at once with a filter. A file an unfinished job still needs is kept (`409`,
  or listed under `skipped`).
- **Let the job clean up after itself** with `cleanup` in the job spec:

  ```yaml
  name: encode-batch
  cleanup:
    after_download: true   # once the job succeeded and every result was downloaded in full
    after_hours: 48        # or 48 hours after it finished (any final state), whichever comes first
    # keep_inputs: true    # keep the uploaded inputs
    # keep_outputs: true   # keep the outputs
    # delete_job: true     # delete the job record too
  tasks: [...]
  ```

  *Results* are the stored outputs that no other task of the job consumes (a reduce's input is not a result).
  A download counts once every byte of the file was served (parallel ranges add up), or when a direct
  download from S3 is confirmed (`POST .../downloaded`, which `umm download` and the SDK send once the
  checksum passes; see [Direct downloads](#direct-downloads-s3-backend)); cleanup then waits two quiet
  minutes. Fetching a presigned URL with another tool does not count until you confirm it. Failed and cancelled jobs are only cleaned by `after_hours`, so you can retry them with the
  same inputs. Inputs are only deleted when the job's owner uploaded them and no unfinished job uses them.
  The job shows `cleaned_at` afterwards; the activity log records `job.cleaned`.

Every delete is written to the activity log with who did it (`job.deleted`, `jobs.purged`, `files.deleted`).

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
  history). The Mac's **Placement** card shows its agent and router labels, its reservation and how many
  tasks it runs at once; admins change them with **Edit**. Reserved Macs carry a *reserved* badge in the
  list. Install links are listed below the Macs; used, expired and revoked ones can be deleted one by one
  or with **Clear old**.
- **Jobs**: filter by state; open a job for its timeline (every attempt of every task on the Mac that ran it,
  by task or by Mac, coloured by outcome), its tasks, and a task's attempts, logs, placement scores and why it
  is still waiting. Admins can cancel a job and delete jobs (a running one is cancelled first; optionally with
  its input files), tick several, or **Delete old jobs** for everything finished more than N days ago.
- **Activity**: enrolments, yields, quarantines, failed jobs, agent update results and every admin change
  (created, revoked and deleted keys, deleted Macs, jobs and install links, retention changes), with who did
  it.
- **API keys** (admins): create and revoke keys; delete revoked ones (one by one or all at once).
- **Settings**: scheduler weights and limits, agent updates, and **Data and retention**: how long 1-minute
  and 15-minute metrics and the activity log are kept, whether finished jobs are deleted automatically, and
  one-off cleanup buttons.

The CLI has the same cleanup: `umm keys delete ID` / `umm keys purge`, `umm install-tokens delete ID` /
`umm install-tokens purge`, `umm macs delete MAC`, `umm jobs rm JOB` / `umm jobs rm -finished -older-than 30d
[-state failed]` (or the older `umm jobs delete` / `umm jobs purge`), `umm files rm`, plus `umm metrics`,
`umm events [-kind mac.]` and `umm retention [set -jobs-days 90]`.

## API reference

All endpoints take and return JSON unless noted. Authenticate with `Authorization: Bearer umm_key_...`.

| Method and path | Role | Does |
| --- | --- | --- |
| `GET /v1/me` | any | who you are, your role and the router's `storage` backend |
| `POST /v1/jobs` | app, admin | submit a job (body: job spec); returns the job (`201`), or the existing one (`200`, `existing: true`) for a repeated `client_id` / `Idempotency-Key` |
| `GET /v1/jobs?state=&limit=` | any | list jobs (apps see only their own) |
| `GET /v1/jobs/{id}` | any | a job, its tasks and state counts |
| `POST /v1/jobs/{id}/cancel` | app, admin | cancel a job |
| `DELETE /v1/jobs/{id}?inputs=true` | app (own), admin | delete a job with its tasks, logs and stored outputs, cancelling it first if it runs; `inputs` also deletes its owner's input files not used by an unfinished job |
| `POST /v1/jobs/purge` | app (own), admin | `{"older_than_days": 0, "states": ["failed"], "inputs": true, "owner": "key:..."}` → `{"deleted": n}`; apps always purge only their own jobs, `owner` is for admins |
| `GET /v1/files?kind=&job=&prefix=&owner=&limit=` | app, admin | stored files you own (`owner`: admins may name another owner or `*`), with `in_use` |
| `DELETE /v1/files/{key}` | app (own), admin | delete one stored file (`uploads/...`, `jobs/...` or a blob key); `409` while an unfinished job needs it |
| `POST /v1/files/delete` | app (own), admin | `{"keys": [...]}` or a filter `{"prefix", "kind", "job_id", "all": true}` → `{deleted, bytes, skipped}` |
| `GET /v1/tasks/{id}` | any | a task with its attempts and placement scores |
| `GET /v1/tasks/{id}/logs?attempt=N` | any | logs as plain text (all attempts when `attempt` is 0 or missing) |
| `GET /v1/tasks/{id}/outputs/{name}` | any | download an output; `?direct=1[&ttl=S][&redirect=1]` (owner, admin; `s3` backend) returns a presigned URL `{url, expires_at, size, sha256}` or a `307` to it |
| `POST /v1/tasks/{id}/outputs/{name}/downloaded` | app (own), admin | confirm a complete direct download `{"sha256"}` (optional, `409` on a mismatch) so `after_download` counts it → `204` |
| `PUT /v1/blobs/{key}` | app, admin | upload an input (raw body) |
| `POST /v1/uploads` | app, admin | start a multipart upload `{key, size, part_size?, direct?}` → `{id, parts, part_size, urls}` (`201`) |
| `GET /v1/uploads/{id}` | app, admin | parts already stored (`done`) and fresh URLs for the rest |
| `PUT /v1/uploads/{id}/parts/{n}` | app, admin | store part `n` (raw body, exactly `part_size` bytes except the last) |
| `POST /v1/uploads/{id}/complete` | app, admin | assemble the blob once every part is stored → `{blob, size, parts}` |
| `DELETE /v1/uploads/{id}` | app, admin | abort an upload and drop its parts |
| `GET /v1/jobs/{id}/stream` | any | server-sent events: `job` (the job with its tasks) on every change, then `end`, or `removed` |
| `GET /v1/jobs/stream?state=&limit=` | any | server-sent events: `jobs` (the list), then `job` and `removed` as jobs change |
| `GET /v1/blobs/{key}` | app, admin | download a blob; `?direct=1` as for outputs |
| `GET /v1/macs` | any | Macs with state and live telemetry |
| `PATCH /v1/macs/{id}` | admin | `{"drain": true}`, `{"opted_out": true}`, `{"name": "Lab Mac 2"}` or `{"max_tasks": 4}` (0 = the agent's own) |
| `PUT /v1/macs/{id}/labels` | admin | `{"labels": {"class": "studio"}}` replaces the router-side labels |
| `PUT` / `DELETE /v1/macs/{id}/reservation` | admin | `{"owners": ["key_..."], "hours": 48, "note": "..."}` (or `until`, RFC 3339) reserves the Mac; `DELETE` removes it |
| `DELETE /v1/macs/{id}` | admin | delete a Mac: revokes its credential and removes its metrics history |
| `POST /v1/enrolment-tokens` | admin | one-time token for a new Mac |
| `GET` / `POST /v1/api-keys`, `DELETE /v1/api-keys/{id}` | admin | list, create (`{"name", "role"}`) and revoke API keys (`purge` below deletes them) |
| `GET /v1/runtimes` | any | managed runtime catalog with each package's state per Mac |
| `PUT /v1/runtimes` | admin | publish or replace a package: `{"manifest", "signature", "prewarm"}` |
| `DELETE /v1/runtimes/{name}` | admin | withdraw a package |
| `PUT /v1/runtimes/{name}/prewarm` | admin | `{"prewarm": true}`: every Mac that can install it does so in the background |
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
| Task stays `pending` | the task's **Placement** tab in the dashboard: every Mac and why it was skipped; its `waiting` field | no Mac has the runtime, not enough RAM or disk, labels don't match, all Macs busy, yielding, drained or offline; only Macs reserved for other owners fit; its pinned `macs` are busy or unavailable |
| `403 ... belongs to another owner` | the blob key | another app key uploaded that blob; use your own key or a different blob key |
| `409 ... still needs it` deleting a file | `umm files` (`IN USE`) | an unfinished job reads it; cancel or delete that job first |
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
| A `mise` task fails before it runs | the task's error and `umm logs TASK` (mise's output) | the config has more than `[tools]`, a tool is missing from `mise.lock` (or has no entry for `macos-arm64`), a checksum does not match, the program is not one the tools provide, or the download host is blocked by `task_network: deny` |
| Attempts with outcome `unavailable` (exit 127) | `umm logs TASK`, the **Activity** page (`mac.runtime_unavailable`) | the Mac could not execute the runtime. The task moved to another Mac without using a retry, and that Mac lost the runtime until its program is fixed |
| Mac stays on an old version | its update state and history (click the version on the **Macs** page) | updates paused, a downgrade without a pinned version (`refused`), a version that rolled back (retried after 1 h, then 2 h, ... up to a day), or "no updater" on Macs installed before self-update |
