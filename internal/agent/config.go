package agent

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/alee-ibrahim/use-my-mac/internal/hostprep"
)

// Config is the agent's YAML configuration. It holds no secrets: the agent
// credential lives in CredentialFile (mode 0600) or UMM_AGENT_CREDENTIAL.
type Config struct {
	Server         string `yaml:"server"`
	Name           string `yaml:"name"`
	CredentialFile string `yaml:"credential_file"`
	WorkDir        string `yaml:"work_dir"`
	MaxTasks       int    `yaml:"max_tasks"`
	// MaxTasksLimit is the hard cap on concurrency. An admin can set a Mac's
	// max_tasks on the router; the agent runs at most this many at once
	// whatever the router asks (default 8, never below max_tasks).
	MaxTasksLimit int `yaml:"max_tasks_limit"`
	// Runtimes is the allowlist: name -> fixed argv prefix. Tasks pick a
	// runtime by name and can only append arguments.
	Runtimes map[string][]string `yaml:"runtimes"`
	Labels   map[string]string   `yaml:"labels"`
	// TaskEnv is added to every task's environment.
	TaskEnv map[string]string `yaml:"task_env"`

	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	PollWait          time.Duration `yaml:"poll_wait"`
	// PreflightInterval is how often the agent re-checks that each
	// runtime's program exists and is executable (also at startup and on
	// SIGHUP), so a program installed later is picked up without a restart.
	// Default 1m.
	PreflightInterval time.Duration `yaml:"preflight_interval"`
	// StopGrace is how long a task gets after SIGTERM before SIGKILL.
	StopGrace time.Duration `yaml:"stop_grace"`

	// Priority: "adaptive" (default: utility while nobody is using the Mac,
	// background while someone is; see AdaptiveQoS), "background"
	// (taskpolicy -b: lowest CPU/IO priority; on Apple Silicon this also
	// keeps work on efficiency cores), "utility" (taskpolicy -c utility +
	// nice), or "none".
	Priority string `yaml:"priority"`
	// PriorityIdle (adaptive only): a logged-in, unlocked console user who
	// has not touched the keyboard or mouse for this long counts as away,
	// so tasks run at utility. Default 5m.
	PriorityIdle time.Duration `yaml:"priority_idle"`
	// Sandbox is no longer supported (sandbox-exec is deprecated). An old
	// config that sets it gets a warning and it is ignored; isolation comes
	// from the _umm user, the home-directory check, the work volume and the
	// optional network policy instead.
	Sandbox *bool `yaml:"sandbox"`
	// Warnings are config problems that do not stop the agent (logged at
	// startup).
	Warnings []string `yaml:"-"`

	// WorkVolume is the dedicated APFS volume mounted at WorkDir, created
	// by the root installer/updater (macOS).
	WorkVolume WorkVolumeConfig `yaml:"work_volume"`
	// MinFreeDisk is the disk space always kept free for the Mac's user.
	// Below it the Mac takes no work, and running tasks are stopped (a Mac
	// fault, not a task failure).
	MinFreeDisk MinFreeDiskConfig `yaml:"min_free_disk"`
	// TaskNetwork: "allow" (default) or "deny": the agent's user (and so
	// every task) may only reach the router and TaskNetworkAllow, enforced
	// by a pf anchor the root updater loads. The Mac refuses work while
	// "deny" is set but not in force.
	TaskNetwork      string   `yaml:"task_network"`
	TaskNetworkAllow []string `yaml:"task_network_allow"`
	// HomeCheck: the agent refuses work while any user home directory is
	// readable or traversable by its own user.
	HomeCheck HomeCheckConfig `yaml:"home_check"`
	// HostStatusFile is written by the root side (`umm-agent prepare` and
	// the updater) and read by the agent. Default: host.json next to the
	// config file.
	HostStatusFile string `yaml:"host_status_file"`
	// PauseFile: while this file exists the Mac takes no work and stops
	// running tasks (a local opt-out switch for the Mac's owner).
	PauseFile string `yaml:"pause_file"`
	// UserLoadScope: "console" or "all" (see telemetry.Options).
	UserLoadScope string `yaml:"user_load_scope"`

	Yield YieldConfig `yaml:"yield"`

	// MaxLogBytes caps the log volume streamed per attempt.
	MaxLogBytes int `yaml:"max_log_bytes"`

	// Update configures the root updater (`umm-agent update`). The agent
	// itself ignores it.
	Update UpdateConfig `yaml:"update"`

	// ProgressParsers picks how a runtime's output is read for progress:
	// "auto" (default: the ffmpeg parser when the executable is ffmpeg),
	// "ffmpeg" or "none". UMM_PROGRESS=<n> lines and $UMM_PROGRESS_FILE
	// always work.
	ProgressParsers map[string]string `yaml:"progress_parsers"`

	// Transfer tunes input downloads and output uploads.
	Transfer TransferConfig `yaml:"transfer"`

	// ManagedRuntimes lets the agent install signed runtime packages from
	// the router's catalog by itself when a task needs one.
	ManagedRuntimes ManagedRuntimesConfig `yaml:"managed_runtimes"`

	// Mise lets tasks bring a pinned toolchain (runtime "mise"). Off by
	// default; see mise.go.
	Mise MiseConfig `yaml:"mise"`
}

// WorkVolumeConfig configures the dedicated work volume.
type WorkVolumeConfig struct {
	// Enabled: nil = true on macOS.
	Enabled *bool `yaml:"enabled"`
	// Name of the APFS volume. Default umm-work.
	Name string `yaml:"name"`
	// Quota: auto (default: the disk's size minus the min_free_disk
	// reserve) or a fixed size such as 400g. Applied when the volume is
	// created.
	Quota string `yaml:"quota"`
}

// On reports whether the work volume is wanted.
func (w WorkVolumeConfig) On() bool { return w.Enabled == nil || *w.Enabled }

// MinFreeDiskConfig is the reserve kept free for the Mac's user: the larger
// of Min and Percent of the system disk.
type MinFreeDiskConfig struct {
	// Min, e.g. 50GiB. Default 50GiB on macOS, 0 elsewhere.
	Min string `yaml:"min"`
	// Percent of the disk. Default 15 on macOS, 0 elsewhere.
	Percent *float64 `yaml:"percent"`

	MinBytes uint64 `yaml:"-"`
}

// HomeCheckConfig configures the home-directory exposure check.
type HomeCheckConfig struct {
	// Root holding the home directories. Default /Users on macOS, "" (off)
	// elsewhere.
	Root string `yaml:"root"`
	// Ignore lists entries of Root that are not homes. Default [Shared].
	Ignore []string `yaml:"ignore"`
	// Disabled turns the check off.
	Disabled bool `yaml:"disabled"`
}

// TransferConfig sets chunked transfer parameters.
type TransferConfig struct {
	// PartSizeMB is the size of each upload part / download range
	// (default 8, minimum 5).
	PartSizeMB int `yaml:"part_size_mb"`
	// Concurrency is the number of parts or ranges in flight (default 4).
	Concurrency int `yaml:"concurrency"`
}

// PartSize in bytes.
func (t TransferConfig) PartSize() int64 { return int64(t.PartSizeMB) << 20 }

// ManagedRuntimesConfig controls on-demand runtime installation. agent.yaml
// is root-owned, so the unprivileged agent cannot widen what it trusts.
type ManagedRuntimesConfig struct {
	// Enabled: nil/true = install catalog packages on demand; false = only
	// the static runtimes allowlist is used.
	Enabled *bool `yaml:"enabled"`
	// Dir is the install prefix. It is owned by root and read-only to the
	// agent's user: packages are installed by the root helper
	// (`umm-agent runtimes-sync`), never by the agent. Default:
	// umm-runtimes next to the agent's own directory, e.g.
	// /usr/local/var/umm-runtimes for work_dir /usr/local/var/umm/work. It
	// must not be inside the work dir's parent, which the agent's user owns.
	Dir string `yaml:"dir"`
	// RequestFile is where the agent asks the root helper for installs and
	// reinstalls. The helper's LaunchDaemon watches it. Default:
	// runtime-requests.json in the work dir's parent.
	RequestFile string `yaml:"request_file"`
	// RequestTimeout: a requested install not done by then counts as
	// failed (and is retried after retry_after). Default 15m.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// InUseFile lists the versions running tasks use, so the root helper
	// keeps them. Default: runtime-in-use.json next to request_file.
	InUseFile string `yaml:"in_use_file"`
	// OnRequest runs after the agent wrote a request (tests use it to run
	// the root helper in-process, as launchd's WatchPaths does on a Mac).
	OnRequest func() `yaml:"-"`
	// Allow lists the package names this Mac may install. Default ["*"]:
	// any package signed by a trusted key.
	Allow []string `yaml:"allow"`
	// PublicKeys are the minisign public keys packages must be signed with.
	// Default: the release key (update.public_key, else the key built into
	// the agent), which already decides what the agent itself runs.
	PublicKeys []string `yaml:"public_keys"`
	// RetryAfter: a failed install is retried after this long. Default 10m.
	RetryAfter time.Duration `yaml:"retry_after"`
}

// On reports whether managed runtimes are enabled.
func (m ManagedRuntimesConfig) On() bool { return m.Enabled == nil || *m.Enabled }

// UpdateConfig is read by the root updater. agent.yaml is root-owned, so the
// unprivileged agent cannot change where updates come from or which key
// they must be signed with.
type UpdateConfig struct {
	// Enabled: nil/true = the updater acts on the router's target version;
	// false = it never changes the binary.
	Enabled *bool `yaml:"enabled"`
	// Source: auto (default: through the router when it relays, else
	// direct), router, or direct.
	Source string `yaml:"source"`
	// ReleaseURL is the direct download base: <release_url>/<version>/<file>.
	// Default: the project's GitHub releases.
	ReleaseURL string `yaml:"release_url"`
	// PublicKey overrides the minisign public key built into the binary
	// (for forks that sign their own releases).
	PublicKey string `yaml:"public_key"`
	// VerifyTimeout is how long a new agent has to heartbeat before the
	// updater rolls back. Default 2m.
	VerifyTimeout time.Duration `yaml:"verify_timeout"`
}

// UpdatesEnabled reports whether the updater may change the binary.
func (u UpdateConfig) UpdatesEnabled() bool { return u.Enabled == nil || *u.Enabled }

var runtimeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// LoadConfig reads and validates a YAML config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.HostStatusFile == "" {
		c.HostStatusFile = filepath.Join(filepath.Dir(path), "host.json")
	}
	return c, c.Normalize()
}

// Normalize fills defaults and validates.
func (c *Config) Normalize() error {
	if c.Server == "" {
		return errors.New("server is required")
	}
	c.Server = strings.TrimRight(c.Server, "/")
	if c.Name == "" {
		h, _ := os.Hostname()
		c.Name = h
	}
	if c.WorkDir == "" {
		return errors.New("work_dir is required")
	}
	if c.MaxTasks <= 0 {
		c.MaxTasks = 1
	}
	if c.MaxTasksLimit <= 0 {
		c.MaxTasksLimit = 8
	}
	if c.MaxTasksLimit < c.MaxTasks {
		c.MaxTasksLimit = c.MaxTasks
	}
	agentDir := path.Dir(path.Clean(filepath.ToSlash(c.WorkDir)))
	if err := c.Mise.normalize(path.Clean(filepath.ToSlash(c.WorkDir))); err != nil {
		return err
	}
	// The runtime name mise belongs to mise toolchains, which only run
	// through the protected path (mise_run.go): never as a plain runtime.
	if rt, ok := c.Runtimes[MiseRuntime]; ok && !(c.Mise.Enabled && len(rt) == 1 && rt[0] == c.Mise.Binary) {
		return errors.New("the runtime name mise is reserved for mise toolchains: set mise.enabled and mise.binary " +
			"(or publish mise as a managed runtime) instead of a runtimes entry")
	}
	if c.Mise.Enabled {
		// Inside work_dir only as a dot-directory directly in it: task dirs
		// are the other entries, and startup empties those.
		wd := path.Clean(filepath.ToSlash(c.WorkDir))
		if d := path.Clean(filepath.ToSlash(c.Mise.CacheDir)); d == wd ||
			(strings.HasPrefix(d, wd+"/") && !strings.HasPrefix(strings.TrimPrefix(d, wd+"/"), ".")) {
			return errors.New("mise.cache_dir inside work_dir must be a dot-directory in it, e.g. " + wd + "/.mise-cache")
		}
		if c.Mise.Binary != "" {
			if c.Runtimes == nil {
				c.Runtimes = map[string][]string{}
			}
			c.Runtimes[MiseRuntime] = []string{c.Mise.Binary}
		}
	}
	if len(c.Runtimes) == 0 && !c.ManagedRuntimes.On() {
		return errors.New("at least one runtime must be allowlisted (or managed_runtimes enabled)")
	}
	for name, argv := range c.Runtimes {
		if !runtimeNameRe.MatchString(name) {
			return fmt.Errorf("runtime name %q is invalid", name)
		}
		if len(argv) == 0 || !strings.HasPrefix(argv[0], "/") {
			return fmt.Errorf("runtime %q must start with an absolute executable path", name)
		}
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 5 * time.Second
	}
	if c.PreflightInterval <= 0 {
		c.PreflightInterval = time.Minute
	}
	if c.PollWait <= 0 {
		c.PollWait = 25 * time.Second
	}
	if c.StopGrace <= 0 {
		c.StopGrace = 10 * time.Second
	}
	switch c.Priority {
	case "":
		c.Priority = PriorityAdaptive
	case PriorityAdaptive, "background", "utility", "none":
	default:
		return fmt.Errorf("priority must be adaptive, background, utility or none")
	}
	if c.PriorityIdle <= 0 {
		c.PriorityIdle = 5 * time.Minute
	}
	if c.UserLoadScope == "" {
		c.UserLoadScope = "console"
	}
	// Fill unset yield fields individually so a partial config keeps sane
	// defaults (YieldOnBattery defaults to true only when the block is absent).
	d := DefaultYieldConfig()
	if c.Yield == (YieldConfig{}) {
		c.Yield = d
	}
	y := &c.Yield
	if y.UserCPUHigh <= 0 {
		y.UserCPUHigh = d.UserCPUHigh
	}
	if y.UserCPULow <= 0 || y.UserCPULow > y.UserCPUHigh {
		y.UserCPULow = y.UserCPUHigh / 2
	}
	if y.SustainSeconds <= 0 {
		y.SustainSeconds = d.SustainSeconds
	}
	if y.CooldownSeconds <= 0 {
		y.CooldownSeconds = d.CooldownSeconds
	}
	if y.MemoryPressure == "" {
		y.MemoryPressure = d.MemoryPressure
	}
	if y.Thermal == "" {
		y.Thermal = d.Thermal
	}
	if c.MaxLogBytes <= 0 {
		c.MaxLogBytes = 1 << 20
	}
	for rt, mode := range c.ProgressParsers {
		switch mode {
		case ParserAuto, ParserFFmpeg, ParserNone:
		default:
			return fmt.Errorf("progress_parsers.%s must be auto, ffmpeg or none", rt)
		}
	}
	if c.Transfer.PartSizeMB == 0 {
		c.Transfer.PartSizeMB = 8
	}
	if c.Transfer.PartSizeMB < 5 || c.Transfer.PartSizeMB > 5120 {
		return fmt.Errorf("transfer.part_size_mb must be between 5 and 5120")
	}
	if c.Transfer.Concurrency <= 0 {
		c.Transfer.Concurrency = 4
	}
	if c.Transfer.Concurrency > 32 {
		c.Transfer.Concurrency = 32
	}
	switch c.Update.Source {
	case "":
		c.Update.Source = "auto"
	case "auto", "router", "direct":
	default:
		return fmt.Errorf("update.source must be auto, router or direct")
	}
	if c.Update.VerifyTimeout <= 0 {
		c.Update.VerifyTimeout = 2 * time.Minute
	}
	mr := &c.ManagedRuntimes
	if mr.Dir == "" {
		mr.Dir = path.Join(path.Dir(agentDir), "umm-runtimes")
	}
	if !filepath.IsAbs(mr.Dir) && !strings.HasPrefix(mr.Dir, "/") {
		return errors.New("managed_runtimes.dir must be absolute")
	}
	inside := func(d, dir string) bool { return d == dir || strings.HasPrefix(d, strings.TrimSuffix(dir, "/")+"/") }
	if d := path.Clean(filepath.ToSlash(mr.Dir)); inside(d, path.Clean(filepath.ToSlash(c.WorkDir))) ||
		(agentDir != "/" && inside(d, agentDir)) {
		return errors.New("managed_runtimes.dir must not be inside " + agentDir +
			" (the agent's user owns it and could swap root's files)")
	}
	if mr.RequestFile == "" {
		mr.RequestFile = path.Join(agentDir, "runtime-requests.json")
	}
	if mr.InUseFile == "" {
		mr.InUseFile = path.Join(path.Dir(filepath.ToSlash(mr.RequestFile)), "runtime-in-use.json")
	}
	if mr.RequestTimeout <= 0 {
		mr.RequestTimeout = 15 * time.Minute
	}
	if len(mr.Allow) == 0 {
		mr.Allow = []string{"*"}
	}
	if mr.RetryAfter <= 0 {
		mr.RetryAfter = 10 * time.Minute
	}
	return c.normalizeIsolation(runtime.GOOS)
}

func (c *Config) normalizeIsolation(goos string) error {
	if c.Sandbox != nil && *c.Sandbox {
		c.Warnings = append(c.Warnings, "sandbox: true is no longer supported (sandbox-exec is deprecated) and is ignored; "+
			"see docs/design.md \"Task isolation\"")
	}
	mac := goos == "darwin"
	if c.WorkVolume.Name == "" {
		c.WorkVolume.Name = "umm-work"
	}
	if c.WorkVolume.Quota == "" {
		c.WorkVolume.Quota = "auto"
	}
	if c.WorkVolume.On() {
		if err := hostprep.ValidVolume(hostprep.VolumeConfig{Name: c.WorkVolume.Name, Quota: c.WorkVolume.Quota}); err != nil {
			return err
		}
	}
	if c.MinFreeDisk.Min == "" && mac {
		c.MinFreeDisk.Min = "50GiB"
	}
	if c.MinFreeDisk.Min != "" {
		n, err := ParseSize(c.MinFreeDisk.Min)
		if err != nil {
			return fmt.Errorf("min_free_disk.min: %w", err)
		}
		c.MinFreeDisk.MinBytes = n
	}
	if c.MinFreeDisk.Percent == nil {
		p := 0.0
		if mac {
			p = 15
		}
		c.MinFreeDisk.Percent = &p
	}
	if p := *c.MinFreeDisk.Percent; p < 0 || p >= 90 {
		return fmt.Errorf("min_free_disk.percent must be between 0 and 90")
	}
	switch c.TaskNetwork {
	case "":
		c.TaskNetwork = "allow"
	case "allow", "deny":
	default:
		return fmt.Errorf("task_network must be allow or deny")
	}
	if c.HomeCheck.Root == "" && mac && !c.HomeCheck.Disabled {
		c.HomeCheck.Root = "/Users"
	}
	if c.HomeCheck.Ignore == nil {
		c.HomeCheck.Ignore = []string{"Shared"}
	}
	return nil
}

// Reserve returns the hostprep reserve settings.
func (c *Config) Reserve() hostprep.ReserveConfig {
	r := hostprep.ReserveConfig{MinBytes: c.MinFreeDisk.MinBytes}
	if c.MinFreeDisk.Percent != nil {
		r.Percent = *c.MinFreeDisk.Percent
	}
	return r
}

// HostPrep returns the root-side preparation settings. The agent's user is
// whoever owns the credential file (the installers make it _umm's).
func (c *Config) HostPrep(stateDir string) (hostprep.Config, error) {
	uid, gid := hostprep.Owner(c.CredentialFile)
	if uid <= 0 {
		return hostprep.Config{}, fmt.Errorf("cannot tell the agent's user: %s must exist and be owned by it", c.CredentialFile)
	}
	allow := append([]string{}, c.TaskNetworkAllow...)
	if u, err := url.Parse(c.Server); err == nil && u.Hostname() != "" {
		allow = append([]string{u.Hostname()}, allow...)
	}
	return hostprep.Config{
		WorkDir: c.WorkDir, AgentUID: uid, AgentGID: gid,
		Volume: hostprep.VolumeConfig{Enabled: c.WorkVolume.On(), Name: c.WorkVolume.Name, Quota: c.WorkVolume.Quota,
			Reserve: c.Reserve()},
		Network:    hostprep.NetworkConfig{Mode: c.TaskNetwork, Allow: allow},
		StatusFile: c.HostStatusFile, StateDir: stateDir,
	}, nil
}

var sizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGT]I?B?|B)?$`)

// ParseSize parses sizes such as 50GiB, 50G, 512MiB or 1048576 (bytes).
// K/M/G/T are binary (1024-based) with or without the "i".
func ParseSize(s string) (uint64, error) {
	m := sizeRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (use e.g. 50GiB)", s)
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	mult := 1.0
	if m[2] != "" && m[2] != "B" {
		mult = math.Pow(1024, float64(strings.IndexByte("KMGT", m[2][0])+1))
	}
	return uint64(f * mult), nil
}

// RuntimeNames returns the allowlisted runtime names, sorted.
func (c *Config) RuntimeNames() []string {
	var out []string
	for k := range c.Runtimes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LoadCredential reads the agent credential from the environment or file.
func (c *Config) LoadCredential() (string, error) {
	if v := strings.TrimSpace(os.Getenv("UMM_AGENT_CREDENTIAL")); v != "" {
		return v, nil
	}
	if c.CredentialFile == "" {
		return "", errors.New("no credential: set credential_file or UMM_AGENT_CREDENTIAL (run `umm-agent enrol` first)")
	}
	b, err := os.ReadFile(c.CredentialFile)
	if err != nil {
		return "", fmt.Errorf("read credential: %w (run `umm-agent enrol` first)", err)
	}
	return strings.TrimSpace(string(b)), nil
}
