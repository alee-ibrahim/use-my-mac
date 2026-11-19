package agent

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the agent's YAML configuration. It holds no secrets: the agent
// credential lives in CredentialFile (mode 0600) or UMM_AGENT_CREDENTIAL.
type Config struct {
	Server         string `yaml:"server"`
	Name           string `yaml:"name"`
	CredentialFile string `yaml:"credential_file"`
	WorkDir        string `yaml:"work_dir"`
	MaxTasks       int    `yaml:"max_tasks"`
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
	// Sandbox wraps tasks in sandbox-exec so they can only write inside
	// their work dir (macOS only; optional).
	Sandbox bool `yaml:"sandbox"`
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
	if len(c.Runtimes) == 0 {
		return errors.New("at least one runtime must be allowlisted")
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
	return nil
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
