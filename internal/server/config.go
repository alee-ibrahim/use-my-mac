package server

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/engine"
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

	Scheduler engine.Config `yaml:"scheduler"`

	MaxUploadMB   int64  `yaml:"max_upload_mb"`
	SecureCookies *bool  `yaml:"secure_cookies"`
	LogLevel      string `yaml:"log_level"`

	// Filled from the environment.
	SecretKey         string `yaml:"-"`
	BootstrapAdminKey string `yaml:"-"`
}

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
	if c.MaxUploadMB <= 0 {
		c.MaxUploadMB = 20 * 1024
	}
	return nil
}
