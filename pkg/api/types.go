// Package api holds the wire types shared by the server, the agent, the CLI
// and the Go client SDK. Everything here is plain JSON; nothing is
// workload-specific.
package api

import "time"

// ---------------------------------------------------------------------------
// Jobs and tasks (app-facing)
// ---------------------------------------------------------------------------

// JobSpec is what an app submits. A job is a small DAG of tasks: usually a set
// of independent "map" tasks plus an optional "reduce" task that depends on
// them. The app does the splitting; the router schedules, retries and
// reassigns.
type JobSpec struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Tasks  []TaskSpec        `json:"tasks"`
	// Macs pins every task that has no Macs of its own to these Macs (IDs
	// or names; see TaskSpec.Macs).
	Macs []string `json:"macs,omitempty"`
	// Prefer is the soft placement default for tasks without their own.
	Prefer *Prefer `json:"prefer,omitempty"`
	// Cleanup deletes the job's stored inputs and outputs automatically
	// once its results have been downloaded and/or some hours after it
	// finished. nil keeps them until deleted.
	Cleanup *Cleanup `json:"cleanup,omitempty"`
	// ClientID makes submitting idempotent: it is unique per owner, and a
	// job submitted again with the same ClientID (and the same spec) returns
	// the existing job instead of a new one. The Idempotency-Key header
	// sets it too. Up to 200 characters.
	ClientID string `json:"client_id,omitempty"`
}

// ExistingTask is a task that was not created again because a task with
// its ClientID already exists for the owner.
type ExistingTask struct {
	ClientID string `json:"client_id"`
	TaskID   string `json:"task_id"`
	JobID    string `json:"job_id"`
	State    string `json:"state"`
}

// Prefer is a soft placement preference: Macs that match get a score bonus,
// but the others still take work when they are the better (or only) choice.
type Prefer struct {
	// Labels must all match for a Mac to count as preferred.
	Labels map[string]string `json:"labels,omitempty"`
	// Macs (IDs or names) count as preferred.
	Macs []string `json:"macs,omitempty"`
	// Bonus is added to a preferred Mac's score (0-100 scale). 0 means the
	// default, 25.
	Bonus float64 `json:"bonus,omitempty"`
}

// Cleanup is a job's automatic cleanup policy. At least one trigger must be
// set; the first one that fires runs it, once.
type Cleanup struct {
	// AfterDownload runs it once the job has succeeded and every result
	// (each stored output no other task of the job consumes) has been
	// downloaded in full, after a short quiet period. Failed and cancelled
	// jobs are only cleaned by AfterHours, so they can be retried with the
	// same inputs.
	AfterDownload bool `json:"after_download,omitempty"`
	// AfterHours runs it this many hours after the job finished (any final
	// state). 0 = no time limit.
	AfterHours int `json:"after_hours,omitempty"`
	// KeepInputs / KeepOutputs leave those in place. Inputs are the blobs
	// the job's tasks read that the job's owner uploaded; one still used by
	// an unfinished job is kept.
	KeepInputs  bool `json:"keep_inputs,omitempty"`
	KeepOutputs bool `json:"keep_outputs,omitempty"`
	// DeleteJob also deletes the job itself (tasks, attempts, logs).
	DeleteJob bool `json:"delete_job,omitempty"`
}

// TaskSpec describes one unit of work.
type TaskSpec struct {
	// Key identifies the task inside its job (unique per job). Other tasks
	// refer to it in DependsOn and in Input.From.
	Key string `json:"key"`
	// Runtime names an allowlisted runtime configured on the agent
	// (e.g. "ffmpeg"). Tasks can never run arbitrary binaries.
	Runtime string `json:"runtime"`
	// Args are appended to the runtime's fixed argv. Placeholders:
	//   {in:NAME}  absolute path of input NAME
	//   {out:NAME} absolute path where output NAME must be written
	//   {workdir}  the task's private working directory
	Args []string          `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`

	Inputs  []Input  `json:"inputs,omitempty"`
	Outputs []Output `json:"outputs,omitempty"`

	Resources Resources `json:"resources,omitempty"`

	// TimeoutSeconds bounds one attempt. 0 means the server default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// MaxRetries is the number of failed attempts retried before the task
	// fails for good. Yields and lost agents do not count. nil means the
	// server default.
	MaxRetries *int `json:"max_retries,omitempty"`

	// DependsOn lists task keys that must succeed before this task can run.
	DependsOn []string `json:"depends_on,omitempty"`

	// Macs pins the task to these Macs, by ID or name: it only runs on one
	// of them. The owner must be allowed to use each (a Mac reserved for
	// other owners cannot be targeted). Combines with Resources.Labels.
	Macs []string `json:"macs,omitempty"`
	// Prefer: soft placement (a score bonus for matching Macs).
	Prefer *Prefer `json:"prefer,omitempty"`
	// ClientID identifies the task for idempotent submits: unique per owner
	// across jobs. A task whose ClientID already exists is not created
	// again; the response lists it under Job.ExistingTasks.
	ClientID string `json:"client_id,omitempty"`

	// Requires names managed runtime packages (from the router's runtime
	// catalog) that must be installed on the Mac before the task runs; their
	// bin directories go first on the task's PATH. A Runtime that a catalog
	// package provides is required implicitly. Macs install missing packages
	// on demand; tasks still can only run allowlisted or catalog commands.
	Requires []string `json:"requires,omitempty"`
}

// Input is a file placed in the task's in/ directory before it starts.
// Exactly one of URL, Blob or From must be set.
type Input struct {
	Name string `json:"name"`
	// URL is a plain URL supplied by the app (GET).
	URL string `json:"url,omitempty"`
	// Blob is a key previously uploaded to the router's storage.
	Blob string `json:"blob,omitempty"`
	// From references an output of another task in the same job.
	From *OutputRef `json:"from,omitempty"`
}

// OutputRef points at an output of another task in the same job.
type OutputRef struct {
	Task   string `json:"task"`
	Output string `json:"output"`
}

// Output is a file the task must write to out/<Name>.
type Output struct {
	Name string `json:"name"`
	// URL optionally sends the output straight to an app-supplied URL (PUT)
	// instead of the router's storage.
	URL string `json:"url,omitempty"`
	// Optional marks outputs that may legitimately be missing.
	Optional bool `json:"optional,omitempty"`
}

// Resources are scheduling hints and hard requirements.
type Resources struct {
	// RAMMB is the memory the task needs; Macs without that much available
	// headroom are skipped.
	RAMMB int `json:"ram_mb,omitempty"`
	// GPU marks work that uses Metal/VideoToolbox/ANE; GPU utilisation then
	// counts in the score.
	GPU bool `json:"gpu,omitempty"`
	// ExpectedSeconds lets the scorer prefer quiet, powerful Macs for long work.
	ExpectedSeconds int `json:"expected_seconds,omitempty"`
	// DiskMB is scratch space needed in the work dir on top of the inputs
	// and outputs.
	DiskMB int `json:"disk_mb,omitempty"`
	// DiskEstimateMB is the task's total work-space need (inputs, outputs
	// and scratch). Default: twice the size of its inputs (as far as the
	// router knows them: blobs and upstream outputs; plain URLs count 0)
	// plus DiskMB. The router places the task only on a Mac with that much
	// room, and fails it if no Mac could ever fit it.
	DiskEstimateMB int `json:"disk_estimate_mb,omitempty"`
	// Labels must all match the Mac's labels (e.g. {"chip":"m2"}): the
	// agent's own, with labels an admin set on the router taking precedence.
	Labels map[string]string `json:"labels,omitempty"`
}

// Task / job states.
const (
	StateBlocked   = "blocked"   // waiting on dependencies
	StatePending   = "pending"   // runnable, waiting for a Mac
	StateAssigned  = "assigned"  // picked a Mac, not yet fetched by it
	StateRunning   = "running"   // fetched by the agent
	StateSucceeded = "succeeded" // done
	StateFailed    = "failed"    // out of retries
	StateCancelled = "cancelled" // cancelled by the app or an upstream failure
)

// IsTerminal reports whether a task/job state is final.
func IsTerminal(s string) bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// Job is the server's view of a job.
type Job struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Owner      string            `json:"owner"`
	Labels     map[string]string `json:"labels,omitempty"`
	State      string            `json:"state"`
	CreatedAt  time.Time         `json:"created_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
	Counts     map[string]int    `json:"counts"`
	// Percent is the job's overall progress (0-100): finished tasks count
	// 100, running tasks their own Percent, waiting tasks 0.
	Percent float64 `json:"percent"`
	// Phase summarises what the job is doing now: queued, starting,
	// downloading, running, uploading or done.
	Phase string `json:"phase,omitempty"`
	Tasks []Task `json:"tasks,omitempty"`
	// Cleanup is the job's cleanup policy; CleanedAt is when it ran.
	Cleanup   *Cleanup   `json:"cleanup,omitempty"`
	CleanedAt *time.Time `json:"cleaned_at,omitempty"`
	ClientID  string     `json:"client_id,omitempty"`
	// Existing is set on a submit response when nothing new was created:
	// the job (or all its tasks) had been submitted before.
	Existing bool `json:"existing,omitempty"`
	// ExistingTasks lists tasks of a submit that already existed (by
	// TaskSpec.ClientID) and were left out of this job.
	ExistingTasks []ExistingTask `json:"existing_tasks,omitempty"`
}

// Task is the server's view of a task.
type Task struct {
	ID          string     `json:"id"`
	JobID       string     `json:"job_id"`
	Key         string     `json:"key"`
	Spec        TaskSpec   `json:"spec"`
	State       string     `json:"state"`
	Attempt     int        `json:"attempt"`
	Failures    int        `json:"failures"`
	AgentID     string     `json:"agent_id,omitempty"`
	Excluded    []string   `json:"excluded,omitempty"`
	NotBefore   *time.Time `json:"not_before,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Attempts    []Attempt  `json:"attempts,omitempty"`
	OutputsDone []string   `json:"outputs_done,omitempty"`
	// OutputSHA256 maps each uploaded output to its SHA-256 (hex), as the
	// Mac computed it from the file it uploaded.
	OutputSHA256 map[string]string `json:"output_sha256,omitempty"`
	// Phase is queued, starting, downloading, running, uploading or done.
	Phase string `json:"phase,omitempty"`
	// Percent is the task's overall progress (0-100). Downloading inputs
	// covers 0-5, running 5-95 and uploading outputs 95-100.
	Percent float64 `json:"percent"`
	// Progress is the latest live report from the agent running the task
	// (nil when the task is not running or the agent sent none yet).
	Progress *TaskProgress `json:"progress,omitempty"`
	// Waiting explains why a pending task has no Mac yet when the reason is
	// more than a busy fleet (only reserved Macs fit, its pinned Macs are
	// unavailable).
	Waiting string `json:"waiting,omitempty"`
}

// Task phases (live progress).
const (
	PhaseQueued      = "queued"      // waiting for dependencies or a Mac
	PhaseStarting    = "starting"    // handed to a Mac, no report yet
	PhaseDownloading = "downloading" // the Mac is fetching inputs
	PhaseRunning     = "running"     // the process is running
	PhaseUploading   = "uploading"   // the Mac is pushing outputs
	PhaseDone        = "done"        // finished (any terminal state)
)

// TaskProgress is a live progress report from the agent running a task.
// Agents send it at most about once a second while something changes.
type TaskProgress struct {
	Attempt int    `json:"attempt"`
	Phase   string `json:"phase"`
	// Percent is the progress within the phase (0-100), -1 when unknown.
	// While running it comes from the task itself (UMM_PROGRESS=<n> lines
	// on stdout/stderr, the $UMM_PROGRESS_FILE file, or a built-in parser
	// such as the one for ffmpeg).
	Percent float64 `json:"percent"`
	// BytesDone/BytesTotal/BytesPerSec describe transfers (downloading and
	// uploading phases).
	BytesDone   int64   `json:"bytes_done,omitempty"`
	BytesTotal  int64   `json:"bytes_total,omitempty"`
	BytesPerSec float64 `json:"bytes_per_sec,omitempty"`
	Message     string  `json:"message,omitempty"`
	// UpdatedAt is set by the router when the report arrives.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Multipart uploads
// ---------------------------------------------------------------------------

// Part sizes for multipart uploads. S3 needs at least 5 MiB per part
// (except the last) and at most 10,000 parts.
const (
	MinPartSize     = 5 << 20
	DefaultPartSize = 8 << 20
	MaxPartSize     = 5 << 30
	MaxParts        = 10000
)

// UploadRequest starts a multipart upload of a blob (POST /v1/uploads).
type UploadRequest struct {
	// Key is the blob key, as later used in Input.Blob.
	Key  string `json:"key"`
	Size int64  `json:"size"`
	// PartSize defaults to 8 MiB; the router raises it when the file would
	// need more than 10,000 parts.
	PartSize int64 `json:"part_size,omitempty"`
	// Direct asks for part URLs that go straight to object storage
	// (presigned S3 URLs) instead of through the router. The router falls
	// back to relay when its backend cannot do that.
	Direct bool `json:"direct,omitempty"`
}

// Upload is the state of a multipart upload. Clients PUT each part's bytes
// to URLs[n] (parts are numbered from 1; any order, in parallel), then
// POST /v1/uploads/{id}/complete. A URL starting with "/" is on the router
// and needs the usual Authorization; any other URL is presigned and must be
// sent without it. GET /v1/uploads/{id} lists the parts already stored and
// fresh URLs for the rest, which is how an interrupted upload resumes.
type Upload struct {
	ID       string `json:"id"`
	Key      string `json:"key"`
	Size     int64  `json:"size"`
	PartSize int64  `json:"part_size"`
	Parts    int    `json:"parts"`
	Direct   bool   `json:"direct"`
	// Done lists the part numbers already stored.
	Done []int `json:"done"`
	// URLs maps part number to upload URL for every part not yet stored.
	URLs      map[int]string `json:"urls,omitempty"`
	ExpiresAt time.Time      `json:"expires_at"`
}

// PartSizeOf returns the size of part n (1-based) of an upload.
func PartSizeOf(size, partSize int64, n int) int64 {
	off := int64(n-1) * partSize
	if off >= size {
		return 0
	}
	return min(partSize, size-off)
}

// PartCount is the number of parts for size bytes.
func PartCount(size, partSize int64) int {
	if size <= 0 || partSize <= 0 {
		return 0
	}
	return int((size + partSize - 1) / partSize)
}

// DirectDownload is a presigned URL that fetches a stored output or blob
// straight from object storage (GET /v1/tasks/{id}/outputs/{name}?direct=1,
// GET /v1/blobs/{key}?direct=1; s3 backend only). Send a plain GET, with
// Range headers if you like, and no Authorization header. Size and SHA256
// (hex) describe the stored file when the router knows them.
type DirectDownload struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256,omitempty"`
}

// MaxDirectDownloadTTL is the longest a presigned download URL can live
// (the SigV4 limit).
const MaxDirectDownloadTTL = 7 * 24 * time.Hour

// DownloadConfirm tells the router a client fetched an output in full
// outside the router (POST /v1/tasks/{id}/outputs/{name}/downloaded), so
// after_download cleanup can count it. SHA256, when set, must match the
// checksum the Mac recorded.
type DownloadConfirm struct {
	SHA256 string `json:"sha256,omitempty"`
}

// OutputUploadRequest is how an agent starts a multipart upload of a task
// output (POST /v1/agent/tasks/{id}/outputs/{name}/upload).
type OutputUploadRequest struct {
	Attempt  int   `json:"attempt"`
	Size     int64 `json:"size"`
	PartSize int64 `json:"part_size,omitempty"`
}

// Attempt records one try of a task on one Mac.
type Attempt struct {
	Attempt    int        `json:"attempt"`
	AgentID    string     `json:"agent_id"`
	AgentName  string     `json:"agent_name,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Outcome: running, succeeded, failed, yielded, lost, cancelled, timeout.
	Outcome string  `json:"outcome"`
	Error   string  `json:"error,omitempty"`
	Score   float64 `json:"score"`
}

// ---------------------------------------------------------------------------
// Macs (agents)
// ---------------------------------------------------------------------------

// Telemetry is what an agent reports on every heartbeat. Unknown numeric
// values are -1.
type Telemetry struct {
	CollectedAt time.Time `json:"collected_at"`

	// Presence
	ConsoleUser     string  `json:"console_user"` // "" when nobody is logged in at the console
	UserIdleSeconds float64 `json:"user_idle_seconds"`
	ScreenLocked    bool    `json:"screen_locked"`

	// Load, split between the user's processes and ours (agent + tasks).
	CPUCount       int     `json:"cpu_count"`
	UserCPUPercent float64 `json:"user_cpu_percent"` // 0..100 of the whole machine
	OurCPUPercent  float64 `json:"our_cpu_percent"`
	UserMemBytes   uint64  `json:"user_mem_bytes"`
	OurMemBytes    uint64  `json:"our_mem_bytes"`

	MemTotalBytes     uint64 `json:"mem_total_bytes"`
	MemAvailableBytes uint64 `json:"mem_available_bytes"`
	// MemoryPressure: normal, warn, critical (or "" if unknown).
	MemoryPressure string `json:"memory_pressure"`
	// Thermal: nominal, fair, serious, critical (or "" if unknown).
	Thermal string `json:"thermal"`

	OnAC           bool    `json:"on_ac"`
	BatteryPercent int     `json:"battery_percent"` // -1 when there is no battery
	GPUUtilPercent float64 `json:"gpu_util_percent"`
	DiskFreeBytes  uint64  `json:"disk_free_bytes"`

	Chip     string `json:"chip"`
	Model    string `json:"model"`
	GPUCores int    `json:"gpu_cores"`
	OS       string `json:"os"`
}

// AgentState is the agent's own view of whether it accepts work.
const (
	AgentReady    = "ready"    // accepting work
	AgentBusyUser = "yielding" // the user is busy; not accepting, stopping tasks
	AgentCooldown = "cooldown" // recently yielded; waiting for the user to calm down
	AgentDraining = "draining" // admin drain / local opt-out
	// AgentUnhealthy: the Mac is not safe or able to run work (user home
	// directories readable by the agent's user, low disk, work volume not
	// mounted, network policy not applied). Reason says why. It is a Mac
	// fault: running tasks are left alone, new ones are refused.
	AgentUnhealthy = "unhealthy"
)

// Mac is the server's view of an enrolled agent.
type Mac struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Online      bool       `json:"online"`
	LastSeen    *time.Time `json:"last_seen,omitempty"`
	AgentState  string     `json:"agent_state"`
	Drain       bool       `json:"drain"`
	OptedOut    bool       `json:"opted_out"`
	Quarantined *time.Time `json:"quarantined_until,omitempty"`
	Runtimes    []string   `json:"runtimes"`
	// Labels are the effective labels tasks match against: AgentLabels
	// (from the Mac's agent.yaml) overridden by AdminLabels (set on the
	// router by an admin).
	Labels      map[string]string `json:"labels"`
	AgentLabels map[string]string `json:"agent_labels,omitempty"`
	AdminLabels map[string]string `json:"admin_labels,omitempty"`
	// Reservation, when set, limits the Mac to tasks of the listed owners.
	Reservation *MacReservation `json:"reservation,omitempty"`
	// MaxTasks is the effective concurrency: the router's setting
	// (MaxTasksSet, 0 = none) capped by the agent's hard limit
	// (AgentMaxTasks), else the agent's own max_tasks.
	MaxTasks      int        `json:"max_tasks"`
	MaxTasksSet   int        `json:"max_tasks_set,omitempty"`
	AgentMaxTasks int        `json:"agent_max_tasks,omitempty"`
	Running       []string   `json:"running"`
	Telemetry     *Telemetry `json:"telemetry,omitempty"`
	Version       string     `json:"version,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	// Update is the agent self-update status (nil before the updater has
	// ever checked in).
	Update *MacUpdate `json:"update,omitempty"`
	// UnavailableRuntimes are allowlisted runtimes the Mac cannot run, with
	// the reason. Runtimes lists only the ones it can run; the scheduler
	// never places a task needing an unavailable runtime here.
	UnavailableRuntimes []UnavailableRuntime `json:"unavailable_runtimes,omitempty"`
	// StateReason explains AgentState (e.g. why a Mac is unhealthy).
	StateReason string `json:"state_reason,omitempty"`
	// Isolation is how this Mac isolates tasks (nil for older agents or
	// while the Mac is offline).
	Isolation *Isolation `json:"isolation,omitempty"`
	// Packages are the managed runtime packages on the Mac (installed,
	// installing or failed). InstallableRuntimes / InstallablePackages are
	// catalog commands and packages the Mac can install on demand.
	Packages            []RuntimePackageStatus `json:"packages,omitempty"`
	InstallableRuntimes []string               `json:"installable_runtimes,omitempty"`
	InstallablePackages []string               `json:"installable_packages,omitempty"`
}

// Isolation describes how a Mac keeps tasks away from its users' data.
type Isolation struct {
	// WorkVolume: "dedicated" (a quota-limited APFS volume of its own),
	// "system" (a directory on the system volume: creating the volume
	// failed or is turned off) or "" (unknown).
	WorkVolume string `json:"work_volume,omitempty"`
	// WorkVolumeNote says why the work dir is not on a dedicated volume.
	WorkVolumeNote string `json:"work_volume_note,omitempty"`
	// Network: "allow" or "deny" (tasks may only reach the router and the
	// configured hosts; enforced by a pf anchor installed by root).
	Network string `json:"network,omitempty"`
	// HomeDirs: "ok", "exposed" (some are readable by the agent's user; the
	// Mac refuses work) or "unchecked".
	HomeDirs string `json:"home_dirs,omitempty"`
	// WorkSpaceBytes is the disk space available for new work now: free
	// space on the work volume, less the reserve kept for the Mac's user.
	// The router only places a task here if its disk need fits.
	WorkSpaceBytes uint64 `json:"work_space_bytes"`
	// WorkCapacityBytes is the space work could use with no task running
	// (WorkSpaceBytes plus what running tasks use). A task needing more than
	// any Mac's capacity fails instead of waiting forever.
	WorkCapacityBytes uint64 `json:"work_capacity_bytes"`
	// SystemFreeBytes and ReserveBytes: free space on the system volume and
	// the part of it kept for the Mac's user.
	SystemFreeBytes uint64 `json:"system_free_bytes,omitempty"`
	ReserveBytes    uint64 `json:"reserve_bytes,omitempty"`
}

// UnavailableRuntime is an allowlisted runtime that a Mac cannot run.
type UnavailableRuntime struct {
	Name string `json:"name"`
	// Program is the runtime's executable path (argv[0]).
	Program string `json:"program,omitempty"`
	// Reason says why, e.g. "program not found: /opt/homebrew/bin/ffmpeg".
	Reason string `json:"reason"`
	// Source: "preflight" (the agent's own check), "exec" (a task could
	// not execute it; it stays unavailable until the agent's next
	// preflight shows it available again) or "install" (installing the
	// managed runtime that provides it failed).
	Source string     `json:"source,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
}

// Sources of an UnavailableRuntime.
const (
	RuntimeSourcePreflight = "preflight"
	RuntimeSourceExec      = "exec"
	// RuntimeSourceInstall: the managed runtime that provides it failed to
	// install (the agent retries after a backoff).
	RuntimeSourceInstall = "install"
)

// MacReservation reserves a Mac for some owners: it only runs their tasks.
type MacReservation struct {
	// Owners are job owners: "key:<API key ID>" or "user:<subject>". Shown
	// to admins and viewers only.
	Owners []string `json:"owners,omitempty"`
	// OwnerNames maps owners to display names (API key names).
	OwnerNames map[string]string `json:"owner_names,omitempty"`
	// Until ends the reservation (nil = until removed).
	Until *time.Time `json:"until,omitempty"`
	Note  string     `json:"note,omitempty"`
	By    string     `json:"by,omitempty"`
	// Mine is true when the caller is one of the owners.
	Mine bool `json:"mine"`
}

// MacReservationRequest sets a reservation (PUT /v1/macs/{id}/reservation).
type MacReservationRequest struct {
	// Owners: "key:<id>", a bare API key ID ("key_..."), or "user:<subject>".
	Owners []string `json:"owners"`
	// Until or Hours (from now) end it; neither = until removed.
	Until *time.Time `json:"until,omitempty"`
	Hours int        `json:"hours,omitempty"`
	Note  string     `json:"note,omitempty"`
}

// MacLabels replaces a Mac's router-side labels (PUT /v1/macs/{id}/labels).
type MacLabels struct {
	Labels map[string]string `json:"labels"`
}

// MacPatch is used by admins to drain, opt out or rename a Mac.
type MacPatch struct {
	// MaxTasks sets how many tasks the Mac runs at once (1-64; 0 returns to
	// the agent's own max_tasks). The agent caps it at its hard limit
	// (agent.yaml max_tasks_limit), so the Mac's owner keeps the final say.
	MaxTasks *int  `json:"max_tasks,omitempty"`
	Drain    *bool `json:"drain,omitempty"`
	OptedOut *bool `json:"opted_out,omitempty"`
	// Name renames the Mac on the router: 1-64 characters from
	// A-Z a-z 0-9 space . _ - (the installer's rule). The router's name is
	// authoritative after enrolment; the agent never sends it again.
	Name *string `json:"name,omitempty"`
}

// ---------------------------------------------------------------------------
// Agent protocol (agents always connect outbound)
// ---------------------------------------------------------------------------

// EnrolRequest exchanges a one-time enrolment token for an agent credential.
type EnrolRequest struct {
	Token    string            `json:"token"`
	Name     string            `json:"name"`
	Runtimes []string          `json:"runtimes"`
	Labels   map[string]string `json:"labels,omitempty"`
	MaxTasks int               `json:"max_tasks"`
	Version  string            `json:"version"`
}

// EnrolResponse carries the long-lived agent credential (shown once).
type EnrolResponse struct {
	AgentID    string `json:"agent_id"`
	Credential string `json:"credential"`
}

// Heartbeat is sent by the agent every few seconds.
type Heartbeat struct {
	State     string            `json:"state"` // AgentReady, AgentBusyUser, ...
	Reason    string            `json:"reason,omitempty"`
	Telemetry Telemetry         `json:"telemetry"`
	Running   []string          `json:"running"` // task IDs currently executing
	Runtimes  []string          `json:"runtimes"`
	Labels    map[string]string `json:"labels,omitempty"`
	MaxTasks  int               `json:"max_tasks"`
	Version   string            `json:"version"`
	// MaxTasksLimit is the agent's hard cap on concurrency (agent.yaml
	// max_tasks_limit); the router never asks for more. 0 = an older agent
	// that ignores router-set concurrency.
	MaxTasksLimit int `json:"max_tasks_limit,omitempty"`
	// Runtimes above lists only the runtimes that passed the agent's
	// preflight (the program exists, is a regular file and the agent's user
	// may execute it). UnavailableRuntimes are the allowlisted ones that
	// failed it.
	UnavailableRuntimes []UnavailableRuntime `json:"unavailable_runtimes,omitempty"`
	// PreflightID changes every time the agent re-runs its runtime
	// preflight (0 = an agent without preflight). The router clears a
	// runtime it marked unavailable after an exec failure once a newer
	// preflight reports the runtime as available again.
	PreflightID int64 `json:"preflight_id,omitempty"`
	// Isolation reports the Mac's task isolation (see Isolation).
	Isolation *Isolation `json:"isolation,omitempty"`

	// Managed runtimes (see RuntimeCatalog). Runtimes above already include
	// the commands of installed packages. InstallableRuntimes and
	// InstallablePackages are what the agent may install on demand (the
	// package is allowed by agent.yaml, signed by a trusted key and built
	// for this Mac). CatalogRevision is the catalog the agent last loaded.
	Packages            []RuntimePackageStatus `json:"packages,omitempty"`
	InstallableRuntimes []string               `json:"installable_runtimes,omitempty"`
	InstallablePackages []string               `json:"installable_packages,omitempty"`
	CatalogRevision     int64                  `json:"catalog_revision,omitempty"`
}

// HeartbeatResponse tells the agent what to stop.
type HeartbeatResponse struct {
	// MaxTasks is the concurrency the router wants (0 = the agent's own
	// max_tasks). The agent applies min(MaxTasks, its hard limit).
	MaxTasks int      `json:"max_tasks,omitempty"`
	Cancel   []string `json:"cancel,omitempty"` // task IDs to stop now
	Drain    bool     `json:"drain"`
	// DrainReason says why (e.g. "updating"); empty for an admin drain.
	DrainReason string `json:"drain_reason,omitempty"`
	// CatalogRevision is the router's runtime catalog revision; the agent
	// fetches GET /v1/agent/runtimes when it differs from its own.
	CatalogRevision int64 `json:"catalog_revision,omitempty"`
}

// Assignment is a task handed to an agent.
type Assignment struct {
	TaskID         string            `json:"task_id"`
	JobID          string            `json:"job_id"`
	Key            string            `json:"key"`
	Attempt        int               `json:"attempt"`
	Runtime        string            `json:"runtime"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env,omitempty"`
	Inputs         []AssignedInput   `json:"inputs"`
	Outputs        []AssignedOutput  `json:"outputs"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Resources      Resources         `json:"resources"`
	// Requires: managed runtime packages to install before running.
	Requires []string `json:"requires,omitempty"`
}

// AssignedInput is a resolved, short-lived download URL.
type AssignedInput struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// AssignedOutput is a resolved, short-lived upload URL (HTTP PUT).
type AssignedOutput struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Optional bool   `json:"optional,omitempty"`
	// Multipart: the output goes to router storage, so the agent may upload
	// a large file in parallel parts (POST
	// /v1/agent/tasks/{id}/outputs/{name}/upload) instead of one PUT to URL.
	Multipart bool `json:"multipart,omitempty"`
}

// PollResponse returns zero or more assignments.
type PollResponse struct {
	Assignments []Assignment `json:"assignments"`
}

// Task outcomes reported by agents.
const (
	OutcomeRunning   = "running"
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeYielded   = "yielded" // stopped because the Mac's user needed it
	OutcomeLost      = "lost"    // agent vanished
	OutcomeCancelled = "cancelled"
	OutcomeTimeout   = "timeout"
	// OutcomeUnavailable: the Mac could not execute the task's runtime
	// (program missing or not executable, exit 127) or failed it for its
	// own reasons (disk full, unhealthy). That is the Mac's fault, so it
	// does not count against the task's max_retries.
	OutcomeUnavailable = "unavailable"
)

// TaskUpdate is how an agent reports progress/finish of an attempt.
type TaskUpdate struct {
	Attempt  int      `json:"attempt"`
	Outcome  string   `json:"outcome"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Error    string   `json:"error,omitempty"`
	Outputs  []string `json:"outputs,omitempty"` // names uploaded successfully
	// OutputSHA256 maps uploaded outputs to the SHA-256 (hex) of the file.
	OutputSHA256 map[string]string `json:"output_sha256,omitempty"`
	// RuntimeUnavailable is set (with the reason) when the agent could not
	// execute the runtime's program at all (exec ENOENT/EACCES, or exit
	// 127 from the launcher). The router then treats the attempt as a Mac
	// fault. Routers also treat a bare exit code 127 this way.
	RuntimeUnavailable string `json:"runtime_unavailable,omitempty"`
	// MacFault is set (with the reason) when the attempt failed because of
	// the Mac rather than the task: the work volume ran out of space or hit
	// its quota, or the Mac became unhealthy before the task started. The
	// router treats it like RuntimeUnavailable (attempt "unavailable", no
	// failure counted) without marking a runtime unavailable.
	MacFault string `json:"mac_fault,omitempty"`
	// ProgramExit: ExitCode is the task program's own, checked before it
	// ran (a mise toolchain's program), not a launcher's, so a 127 is the
	// task's and not a runtime fault.
	ProgramExit bool `json:"program_exit,omitempty"`
}

// LogChunk is a piece of a task's stdout/stderr.
type LogChunk struct {
	Attempt int    `json:"attempt"`
	Seq     int    `json:"seq"`
	Data    string `json:"data"`
}

// ---------------------------------------------------------------------------
// Managed runtimes
// ---------------------------------------------------------------------------

// RuntimePackage is a managed runtime: a tool the agent downloads, verifies
// and installs by itself (into its own prefix: no Homebrew, no sudo, nothing
// the Mac's user sees) when a task needs it. It is the manifest an admin
// signs with minisign; agents install only packages whose signature
// verifies with a key they trust (agent.yaml managed_runtimes.public_keys,
// else the release key) and that agent.yaml allows.
type RuntimePackage struct {
	// Name of the package, e.g. "ffmpeg".
	Name string `json:"name"`
	// Version, e.g. "7.1". Publishing a new version replaces the old one.
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// Commands are the task runtimes the package provides: runtime name ->
	// argv. argv[0] is a path relative to the package root
	// (e.g. {"ffmpeg": ["bin/ffmpeg"]}); tasks can only append arguments.
	Commands map[string][]string `json:"commands"`
	// Path lists directories (relative to the package root) put first on
	// PATH for tasks that require the package. Default: the directories of
	// the commands.
	Path []string `json:"path,omitempty"`
	// Artifacts are the downloads, one per platform.
	Artifacts []RuntimeArtifact `json:"artifacts"`
}

// RuntimeArtifact is one platform's download of a package.
type RuntimeArtifact struct {
	OS   string `json:"os"`   // GOOS, e.g. darwin
	Arch string `json:"arch"` // GOARCH, e.g. arm64
	// Exactly one of URL (an http(s) URL the Macs can reach) or Blob (a key
	// uploaded to the router's storage with PUT /v1/blobs/KEY) is set.
	URL  string `json:"url,omitempty"`
	Blob string `json:"blob,omitempty"`
	// SHA256 of the download (hex). Size in bytes (optional; enforced).
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
	// Format: tar.gz, zip or file (a single executable, saved as File).
	Format string `json:"format"`
	File   string `json:"file,omitempty"`
}

// Runtime package formats.
const (
	FormatTarGz = "tar.gz"
	FormatZip   = "zip"
	FormatFile  = "file"
)

// RuntimeCatalogEntry is one package in the router's catalog.
type RuntimeCatalogEntry struct {
	Package RuntimePackage `json:"package"`
	// Manifest is the exact signed JSON text; Signature is its .minisig.
	Manifest  string `json:"manifest"`
	Signature string `json:"signature"`
	KeyID     string `json:"key_id"`
	// Prewarm: every Mac that can install the package does so in the
	// background, before any task needs it.
	Prewarm     bool      `json:"prewarm"`
	PublishedAt time.Time `json:"published_at"`
	PublishedBy string    `json:"published_by,omitempty"`
	// Macs is the fleet's state for this package (GET /v1/runtimes only).
	Macs []RuntimeMacStatus `json:"macs,omitempty"`
}

// RuntimeMacStatus is one Mac's state for a catalog package.
type RuntimeMacStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// State: installed, installing, failed (the Mac's own report),
	// outdated (another version is installed) or installable.
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// RuntimeCatalog is what GET /v1/runtimes returns.
type RuntimeCatalog struct {
	Revision int64                 `json:"revision"`
	Packages []RuntimeCatalogEntry `json:"packages"`
}

// RuntimePublish adds or replaces a package (PUT /v1/runtimes).
type RuntimePublish struct {
	Manifest  string `json:"manifest"`
	Signature string `json:"signature"`
	Prewarm   bool   `json:"prewarm,omitempty"`
}

// RuntimePrewarm turns fleet-wide pre-installation of a package on or off.
type RuntimePrewarm struct {
	Prewarm bool `json:"prewarm"`
}

// AgentRuntimeCatalog is what GET /v1/agent/runtimes returns to agents.
type AgentRuntimeCatalog struct {
	Revision int64                 `json:"revision"`
	Packages []AgentRuntimePackage `json:"packages"`
}

// AgentRuntimePackage is a signed package plus resolved download URLs.
type AgentRuntimePackage struct {
	Manifest  string `json:"manifest"`
	Signature string `json:"signature"`
	Prewarm   bool   `json:"prewarm"`
	// URLs[i] is where to download Artifacts[i] (a blob artifact resolved
	// to a short-lived signed URL). Only the signed sha256 decides what the
	// agent accepts.
	URLs []string `json:"urls"`
}

// RuntimePackageStatus is a managed package's state on one Mac.
type RuntimePackageStatus struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// State: installed, installing or failed.
	State       string     `json:"state"`
	Error       string     `json:"error,omitempty"`
	SizeBytes   int64      `json:"size_bytes,omitempty"`
	InstalledAt *time.Time `json:"installed_at,omitempty"`
	// RetryAt: after a failed install, the agent tries again from then.
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

// Package states.
const (
	PackageInstalled   = "installed"
	PackageInstalling  = "installing"
	PackageFailed      = "failed"
	PackageInstallable = "installable"
	PackageOutdated    = "outdated"
)

// ---------------------------------------------------------------------------
// Agent self-update
// ---------------------------------------------------------------------------

// UpdateSettings is the admin's update policy for the fleet.
type UpdateSettings struct {
	// Paused stops all agent updates.
	Paused bool `json:"paused"`
	// TargetVersion is what every Mac should run. "" means the router's own
	// version. Setting it explicitly also allows downgrades to it.
	TargetVersion string `json:"target_version"`
	// CanaryVersion, when set, goes first to the canary Macs: those listed
	// in CanaryMacs plus a stable CanaryPercent share of the fleet. The rest
	// stay on TargetVersion. Promote by setting TargetVersion to it.
	CanaryVersion string   `json:"canary_version"`
	CanaryPercent int      `json:"canary_percent"`
	CanaryMacs    []string `json:"canary_macs"`
	// DrainTimeoutSeconds bounds how long the updater waits for running
	// tasks to finish before it restarts the agent anyway (they are then
	// requeued, not failed). 0 means the default (1 h).
	DrainTimeoutSeconds int `json:"drain_timeout_seconds"`
}

// UpdatesInfo is what GET /v1/updates returns.
type UpdatesInfo struct {
	Settings      UpdateSettings `json:"settings"`
	RouterVersion string         `json:"router_version"`
	// Relay: the router serves release artifacts to agents (cached).
	Relay      bool   `json:"relay"`
	ReleaseURL string `json:"release_url,omitempty"`
}

// UpdateCheck is the router's answer to an updater asking what to run.
type UpdateCheck struct {
	// TargetVersion is the version this Mac should run; "" = leave it alone.
	TargetVersion string `json:"target_version,omitempty"`
	// AllowDowngrade is true only when an admin pinned the version explicitly.
	AllowDowngrade bool `json:"allow_downgrade"`
	Paused         bool `json:"paused"`
	// Reason explains the target (fleet target, canary, router version, ...).
	Reason              string `json:"reason,omitempty"`
	DrainTimeoutSeconds int    `json:"drain_timeout_seconds"`
	// Relay: artifacts can be fetched from /v1/agent/update/artifacts.
	Relay bool `json:"relay"`
	// Agent is the router's live view of this Mac's agent process.
	Agent UpdateAgentView `json:"agent"`
	// NowMS is the router's clock (Unix ms), to compare with Agent.LastSeenMS.
	NowMS int64 `json:"now_ms"`
}

// UpdateAgentView is what the router last heard from the agent itself.
type UpdateAgentView struct {
	Version    string `json:"version"`
	Online     bool   `json:"online"`
	Running    int    `json:"running"`
	LastSeenMS int64  `json:"last_seen_ms"`
}

// Update states reported by the updater.
const (
	UpdateUpToDate    = "up_to_date"
	UpdatePaused      = "paused"
	UpdateDownloading = "downloading"
	UpdateDraining    = "draining"
	UpdateApplying    = "applying"
	UpdateSucceeded   = "succeeded"
	UpdateRolledBack  = "rolled_back"
	UpdateFailed      = "failed"
	UpdateRefused     = "refused"
)

// UpdateReport is sent by the updater on every state change.
type UpdateReport struct {
	State       string `json:"state"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	Message     string `json:"message,omitempty"`
}

// MacUpdate is the latest update status of one Mac.
type MacUpdate struct {
	State       string     `json:"state,omitempty"`
	FromVersion string     `json:"from_version,omitempty"`
	ToVersion   string     `json:"to_version,omitempty"`
	Message     string     `json:"message,omitempty"`
	At          *time.Time `json:"at,omitempty"`
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
	// Desired is the version the router currently wants on this Mac.
	Desired string `json:"desired,omitempty"`
}

// UpdateEvent is one entry of a Mac's update history.
type UpdateEvent struct {
	At          time.Time `json:"at"`
	State       string    `json:"state"`
	FromVersion string    `json:"from_version,omitempty"`
	ToVersion   string    `json:"to_version,omitempty"`
	Message     string    `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// Admin
// ---------------------------------------------------------------------------

// APIKey is an app credential (secret shown once at creation).
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"` // "app" or "admin"
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	Secret     string     `json:"secret,omitempty"`
}

// StoredFile is a blob in router storage with its owner (GET /v1/files).
type StoredFile struct {
	// Key is the storage key: uploads/<blob> for app uploads,
	// jobs/<job>/<task>/<output> for task outputs.
	Key string `json:"key"`
	// Blob is the key to use as Input.Blob (uploads only).
	Blob   string `json:"blob,omitempty"`
	Kind   string `json:"kind"` // upload or output
	Owner  string `json:"owner"`
	JobID  string `json:"job_id,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	Size   int64  `json:"size"`
	// SHA256 of an output (hex), as computed by the Mac that produced it.
	SHA256 string `json:"sha256,omitempty"`
	// CreatedAt is when it was stored; DownloadedAt when it was first
	// downloaded in full (outputs).
	CreatedAt    time.Time  `json:"created_at"`
	DownloadedAt *time.Time `json:"downloaded_at,omitempty"`
	// InUse: an unfinished job still needs it, so it cannot be deleted.
	InUse bool `json:"in_use"`
}

// File kinds.
const (
	FileUpload = "upload"
	FileOutput = "output"
)

// DeleteFilesRequest deletes several stored files (POST /v1/files/delete).
// Give Keys, or a filter (Prefix, Kind, JobID) with All set; files still
// in use by an unfinished job are skipped.
type DeleteFilesRequest struct {
	Keys   []string `json:"keys,omitempty"`
	Prefix string   `json:"prefix,omitempty"`
	Kind   string   `json:"kind,omitempty"`
	JobID  string   `json:"job_id,omitempty"`
	// All confirms a filter-only delete (every matching file of the owner).
	All bool `json:"all,omitempty"`
	// Owner: admins may act on another owner's files ("" = their own, "*" =
	// everyone's).
	Owner string `json:"owner,omitempty"`
}

// DeleteFilesResult reports a bulk file delete.
type DeleteFilesResult struct {
	Deleted int           `json:"deleted"`
	Bytes   int64         `json:"bytes"`
	Skipped []SkippedFile `json:"skipped,omitempty"`
}

// SkippedFile is a file a bulk delete left in place, and why.
type SkippedFile struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// EnrolmentToken is a one-time token used by a new Mac (secret shown once).
type EnrolmentToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Me describes the caller.
type Me struct {
	Subject string `json:"subject"`
	Name    string `json:"name"`
	Role    string `json:"role"` // admin, viewer, app
	Via     string `json:"via"`  // oidc, apikey
	// Storage is the router's blob backend (local, s3). Direct transfers
	// (presigned URLs) need s3.
	Storage string `json:"storage,omitempty"`
}

// Roles.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
	RoleApp    = "app"
)
