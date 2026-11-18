// Command umm-server is the use-my-mac router: REST API, scheduler, storage,
// agent endpoint, metrics and dashboard in one binary.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"crypto/rand"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/engine"
	"github.com/alee-ibrahim/use-my-mac/internal/metrics"
	"github.com/alee-ibrahim/use-my-mac/internal/server"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

var version = "dev"

const usage = `umm-server — use-my-mac router

Usage:
  umm-server [serve] [-config FILE]
  umm-server admin-key create -name NAME [-config FILE]   print a new admin API key once
  umm-server version

Secrets are read from the environment only (see docs/design.md):
  UMM_DATABASE_URL, UMM_SECRET_KEY, UMM_OIDC_CLIENT_SECRET,
  UMM_S3_ACCESS_KEY, UMM_S3_SECRET_KEY, UMM_BOOTSTRAP_ADMIN_KEY
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "admin-key":
		err = adminKey(args)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func configFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("UMM_CONFIG")
	return fs.String("config", def, "config file (YAML); env UMM_CONFIG")
}

func logger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func open(ctx context.Context, cfg *server.Config, log *slog.Logger) (*store.Store, error) {
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	// Signing key: env, else persisted random key (single-replica installs).
	if cfg.SecretKey == "" {
		v, err := st.GetSetting(ctx, "secret_key")
		if errors.Is(err, store.ErrNotFound) {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return nil, err
			}
			v = hex.EncodeToString(b)
			if err := st.PutSetting(ctx, "secret_key", v); err != nil {
				return nil, err
			}
			log.Warn("UMM_SECRET_KEY not set; generated one and stored it in the database")
		} else if err != nil {
			return nil, err
		}
		cfg.SecretKey = v
	}
	return st, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := configFlag(fs)
	fs.Parse(args)
	cfg, err := server.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	log := logger(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Close()

	var blobs storage.Backend
	switch cfg.Storage.Backend {
	case "s3":
		s := cfg.Storage.S3
		blobs = &storage.S3{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: s.Prefix,
			PathStyle: s.PathStyle, AccessKey: s.AccessKey, SecretKey: s.SecretKey}
	default:
		blobs, err = storage.NewLocal(cfg.Storage.Local.Dir, cfg.PublicURL, []byte(cfg.SecretKey))
		if err != nil {
			return err
		}
	}

	m := metrics.New()
	eng, err := engine.New(ctx, engine.Options{Store: st, Blobs: blobs, Metrics: m, Logger: log, Config: cfg.Scheduler, Version: version})
	if err != nil {
		return err
	}
	if cfg.BootstrapAdminKey != "" {
		if n, err := st.CountActiveAdminKeys(ctx); err == nil && n == 0 {
			if err := eng.ImportAPIKey(ctx, "bootstrap-admin", api.RoleAdmin, cfg.BootstrapAdminKey); err != nil {
				return err
			}
			log.Info("bootstrap admin key installed")
		}
	}

	srv := &server.Server{E: eng, Log: log, Metrics: m, SessionKey: []byte(cfg.SecretKey), WebDir: cfg.WebDir,
		MaxUploadBytes: cfg.MaxUploadMB << 20, SecureCookies: strings.HasPrefix(cfg.PublicURL, "https://"),
		Updates: cfg.Updates, PublicURL: cfg.PublicURL, Install: cfg.Install}
	if cfg.SecureCookies != nil {
		srv.SecureCookies = *cfg.SecureCookies
	}
	if cfg.OIDC.Enabled() {
		o, err := auth.NewOIDC(ctx, cfg.OIDC)
		if err != nil {
			return err
		}
		srv.OIDC = o
		log.Info("oidc enabled", "issuer", o.Cfg.Issuer)
	}
	srv.APIKeyLogin = cfg.Auth.APIKeyLoginAllowed(srv.OIDC != nil)
	srv.AutoSignIn = cfg.Auth.AutoSignInEnabled(srv.OIDC != nil)
	log.Info("dashboard auth", "oidc", srv.OIDC != nil, "api_key_login", srv.APIKeyLogin, "auto_sign_in", srv.AutoSignIn)

	go eng.Run(ctx)
	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("umm-server listening", "addr", cfg.Listen, "public_url", cfg.PublicURL, "storage", blobs.Name(), "version", version)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func adminKey(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: umm-server admin-key create -name NAME")
	}
	fs := flag.NewFlagSet("admin-key", flag.ExitOnError)
	cfgPath := configFlag(fs)
	name := fs.String("name", "admin", "key name")
	fs.Parse(args[1:])
	cfg, err := server.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	log := logger("warn")
	st, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Close()
	eng, err := engine.New(ctx, engine.Options{Store: st, Blobs: nil, Logger: log, Config: cfg.Scheduler})
	if err != nil {
		return err
	}
	k, err := eng.CreateAPIKey(ctx, *name, api.RoleAdmin)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Admin API key (shown once; store it in a secret manager):")
	fmt.Println(k.Secret)
	return nil
}
