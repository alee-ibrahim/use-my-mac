package server

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/engine"
	"github.com/alee-ibrahim/use-my-mac/internal/installer"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Config is the router's configuration file. Secrets never live here: they
// come from environment variables (typically a Kubernetes Secret).
//
//	UMM_DATABASE_URL        overrides database (may contain a password)
//	UMM_SECRET_KEY          HMAC key for sessions and signed blob URLs (>= 32 chars)
//	UMM_OIDC_CLIENT_SECRET  OIDC client secret
//	UMM_S3_ACCESS_KEY / UMM_S3_SECRET_KEY
//	UMM_BOOTSTRAP_ADMIN_KEY optional admin API key created on first start
type Config struct {
	Listen string `yaml:"listen"`
	// PublicURL is how agents and browsers reach the router.
	PublicURL string `yaml:"public_url"`
	Database  string `yaml:"database"`
	WebDir    string `yaml:"web_dir"`

	Storage StorageConfig   `yaml:"storage"`
	OIDC    auth.OIDCConfig `yaml:"oidc"`
	Auth    AuthConfig      `yaml:"auth"`

	Scheduler engine.Config `yaml:"scheduler"`

	Updates UpdatesConfig `yaml:"updates"`

	Install InstallConfig `yaml:"install"`

	// Retention is the default retention policy for metrics history, the
	// activity log and finished jobs. Admins can change it at runtime
	// (dashboard Settings or PUT /v1/retention); that choice wins.
	Retention api.Retention `yaml:"retention"`

	MaxUploadMB   int64  `yaml:"max_upload_mb"`
	SecureCookies *bool  `yaml:"secure_cookies"`
	LogLevel      string `yaml:"log_level"`

	// Filled from the environment.
	SecretKey         string `yaml:"-"`
	BootstrapAdminKey string `yaml:"-"`
}

// AuthConfig controls how people sign in to the dashboard. API keys always
// work as Bearer tokens for the REST API, the CLI and agents; these settings
// only affect browser sessions.
type AuthConfig struct {
	// DashboardAPIKeyLogin allows exchanging an API key for a dashboard
	// session: "auto" (default) = only when OIDC is not configured, "true" =
	// always (break-glass alongside OIDC), "false" = never.
	DashboardAPIKeyLogin string `yaml:"dashboard_api_key_login"`
	// AutoSignIn sends signed-out browsers straight to the OIDC provider
	// instead of showing a login page. Default true; ignored without OIDC.
	AutoSignIn *bool `yaml:"auto_sign_in"`
}

// APIKeyLoginAllowed resolves DashboardAPIKeyLogin for the given OIDC state.
func (a AuthConfig) APIKeyLoginAllowed(oidc bool) bool {
	switch strings.ToLower(strings.TrimSpace(a.DashboardAPIKeyLogin)) {
	case "true", "yes", "on":
		return true
	case "false", "no", "off":
		return false
	}
	return !oidc
}

// AutoSignInEnabled resolves AutoSignIn for the given OIDC state.
func (a AuthConfig) AutoSignInEnabled(oidc bool) bool {
	return oidc && (a.AutoSignIn == nil || *a.AutoSignIn)
}

// UpdatesConfig controls how the router helps agents update themselves.
// What version they run is a runtime setting (dashboard Settings).
type UpdatesConfig struct {
	// Relay serves release artifacts to agents at /v1/agent/update/artifacts,
	// cached in the blob storage. Default true.
	Relay *bool `yaml:"relay"`
	// ReleaseURL is where the relay fetches artifacts:
	// <release_url>/<version>/<file>. Default: the project's GitHub releases.
	ReleaseURL string `yaml:"release_url"`
	// Offline: never fetch; serve only artifacts an admin uploaded
	// (PUT /v1/updates/artifacts/<version>/<file>).
	Offline bool `yaml:"offline"`
}

// InstallConfig controls the one-line Mac install (/i/<token>).
type InstallConfig struct {
	// AllowHTTP serves install scripts for a plain-http public_url. Dev only.
	AllowHTTP bool `yaml:"allow_http"`
	// ServiceLabel is the LaunchDaemon label for profiles that set none
	// (default io.github.alee-ibrahim.umm-agent). Use a neutral one, e.g.
	// com.example.helper, for a quiet install.
	ServiceLabel string `yaml:"service_label"`
	// Profiles admins pick from when creating an install link. Without any,
	// a "default" profile allowlists /opt/homebrew/bin/ffmpeg.
	Profiles map[string]InstallProfileConfig `yaml:"profiles"`

	// Resolved by LoadConfig.
	Resolved map[string]api.InstallProfile `yaml:"-"`
}

// InstallProfileConfig is one install profile.
type InstallProfileConfig struct {
	Labels       map[string]string   `yaml:"labels"`
	Runtimes     map[string][]string `yaml:"runtimes"`
	MaxTasks     int                 `yaml:"max_tasks"`
	Priority     string              `yaml:"priority"`
	ServiceLabel string              `yaml:"service_label"`
}

func (c *InstallConfig) resolve() error {
	profiles := c.Profiles
	if len(profiles) == 0 {
		profiles = map[string]InstallProfileConfig{"default": {Runtimes: map[string][]string{"ffmpeg": {"/opt/homebrew/bin/ffmpeg"}}}}
	}
	c.Resolved = map[string]api.InstallProfile{}
	for name, pc := range profiles {
		p := api.InstallProfile{Name: name, Labels: pc.Labels, Runtimes: pc.Runtimes, MaxTasks: pc.MaxTasks,
			Priority: pc.Priority, ServiceLabel: pc.ServiceLabel}
		if p.ServiceLabel == "" {
			p.ServiceLabel = c.ServiceLabel
		}
		if err := installer.NormalizeProfile(&p); err != nil {
			return fmt.Errorf("install.profiles: %w", err)
		}
		c.Resolved[name] = p
	}
	return nil
}

// RelayEnabled resolves Relay.
func (u UpdatesConfig) RelayEnabled() bool { return u.Relay == nil || *u.Relay }

// StorageConfig selects the blob backend.
type StorageConfig struct {
	Backend string `yaml:"backend"` // local (default) or s3
	Local   struct {
		Dir string `yaml:"dir"`
	} `yaml:"local"`
	S3 struct {
		Endpoint  string `yaml:"endpoint"`
		Region    string `yaml:"region"`
		Bucket    string `yaml:"bucket"`
		Prefix    string `yaml:"prefix"`
		PathStyle bool   `yaml:"path_style"`
		AccessKey string `yaml:"-"`
		SecretKey string `yaml:"-"`
	} `yaml:"s3"`
}

// LoadConfig reads an optional YAML file and the environment.
func LoadConfig(path string) (*Config, error) {
	c := &Config{Listen: ":8080", Database: "sqlite://umm.db", Scheduler: engine.DefaultConfig()}
	c.Storage.Backend = "local"
	c.Storage.Local.Dir = "data/blobs"
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	env := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	if v := env("UMM_DATABASE_URL"); v != "" {
		c.Database = v
	}
	if v := env("UMM_PUBLIC_URL"); v != "" {
		c.PublicURL = v
	}
	if v := env("UMM_LISTEN"); v != "" {
		c.Listen = v
	}
	c.SecretKey = env("UMM_SECRET_KEY")
	c.BootstrapAdminKey = env("UMM_BOOTSTRAP_ADMIN_KEY")
	c.OIDC.ClientSecret = env("UMM_OIDC_CLIENT_SECRET")
	c.Storage.S3.AccessKey = env("UMM_S3_ACCESS_KEY")
	c.Storage.S3.SecretKey = env("UMM_S3_SECRET_KEY")
	return c, c.validate()
}

func (c *Config) validate() error {
	if c.PublicURL == "" {
		c.PublicURL = "http://localhost" + c.Listen
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.SecretKey != "" && len(c.SecretKey) < 32 {
		return errors.New("UMM_SECRET_KEY must be at least 32 characters")
	}
	switch c.Storage.Backend {
	case "local", "":
	case "s3":
		if c.Storage.S3.Endpoint == "" || c.Storage.S3.Bucket == "" || c.Storage.S3.AccessKey == "" || c.Storage.S3.SecretKey == "" {
			return errors.New("s3 storage needs endpoint, bucket, UMM_S3_ACCESS_KEY and UMM_S3_SECRET_KEY")
		}
	default:
		return fmt.Errorf("unknown storage backend %q", c.Storage.Backend)
	}
	switch strings.ToLower(strings.TrimSpace(c.Auth.DashboardAPIKeyLogin)) {
	case "", "auto", "true", "false", "yes", "no", "on", "off":
	default:
		return fmt.Errorf("auth.dashboard_api_key_login must be auto, true or false, not %q", c.Auth.DashboardAPIKeyLogin)
	}
	if c.OIDC.Enabled() && c.OIDC.RedirectURL == "" {
		c.OIDC.RedirectURL = c.PublicURL + "/auth/callback"
	}
	s := &c.Scheduler
	d := engine.DefaultConfig()
	if s.TickInterval <= 0 {
		s.TickInterval = d.TickInterval
	}
	if s.HeartbeatTimeout <= 0 {
		s.HeartbeatTimeout = d.HeartbeatTimeout
	}
	if s.AssignTimeout <= 0 {
		s.AssignTimeout = d.AssignTimeout
	}
	if s.DefaultTimeout <= 0 {
		s.DefaultTimeout = d.DefaultTimeout
	}
	if s.MaxLogBytes <= 0 {
		s.MaxLogBytes = d.MaxLogBytes
	}
	if s.EnrolTokenTTL <= 0 {
		s.EnrolTokenTTL = d.EnrolTokenTTL
	}
	if s.BackoffBase <= 0 {
		s.BackoffBase = d.BackoffBase
	}
	if s.BackoffMax <= 0 {
		s.BackoffMax = d.BackoffMax
	}
	if c.Updates.ReleaseURL == "" {
		c.Updates.ReleaseURL = selfupdate.DefaultReleaseURL
	}
	c.Updates.ReleaseURL = strings.TrimRight(c.Updates.ReleaseURL, "/")
	if err := c.Install.resolve(); err != nil {
		return err
	}
	if c.MaxUploadMB <= 0 {
		c.MaxUploadMB = 20 * 1024
	}
	return nil
}
