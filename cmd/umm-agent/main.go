// Command umm-agent runs on each Mac. It connects outbound to the router,
// reports telemetry, runs allowlisted native processes, and gets out of the
// way when the Mac's own user needs the machine.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/agent"
	"github.com/alee-ibrahim/use-my-mac/internal/agent/telemetry"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
)

const usage = `umm-agent — use-my-mac worker

Usage:
  umm-agent run       [-config FILE]   run the agent (launchd does this)
  umm-agent enrol     [-config FILE]   exchange a one-time enrolment token for a credential;
                                       the token is read from UMM_ENROL_TOKEN or stdin
  umm-agent telemetry [-config FILE]   print one telemetry snapshot as JSON (diagnostics)
  umm-agent update    [-config FILE] [-label L] [-jitter D] [-dry-run]
                                       one self-update cycle (root; its own LaunchDaemon runs it)
  umm-agent version

Default config: /usr/local/etc/umm/agent.yaml (env UMM_AGENT_CONFIG)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	def := os.Getenv("UMM_AGENT_CONFIG")
	if def == "" {
		def = "/usr/local/etc/umm/agent.yaml"
	}
	cfgPath := fs.String("config", def, "agent config file")
	verbose := fs.Bool("v", false, "debug logging")
	var err error
	switch cmd {
	case "run":
		fs.Parse(args)
		err = run(*cfgPath, *verbose)
	case "enrol", "enroll":
		fs.Parse(args)
		err = enrol(*cfgPath)
	case "telemetry":
		fs.Parse(args)
		err = snapshot(*cfgPath)
	case "update":
		label := fs.String("label", "io.github.alee-ibrahim.umm-agent", "the agent's LaunchDaemon label")
		bin := fs.String("bin", "/usr/local/bin/umm-agent", "the live agent binary")
		stateDir := fs.String("state-dir", "/usr/local/var/umm-updater", "root-only updater state directory")
		jitter := fs.Duration("jitter", 0, "sleep a random time up to this long first (spreads the fleet)")
		dryRun := fs.Bool("dry-run", false, "download and verify, but do not install")
		fs.Parse(args)
		err = update(*cfgPath, *label, *bin, *stateDir, *jitter, *dryRun, *verbose)
	case "version":
		fmt.Println(agent.Version)
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

func run(path string, verbose bool) error {
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		return err
	}
	cred, err := cfg.LoadCredential()
	if err != nil {
		return err
	}
	lvl := slog.LevelInfo
	if verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("umm-agent starting", "server", cfg.Server, "name", cfg.Name, "runtimes", cfg.RuntimeNames(),
		"max_tasks", cfg.MaxTasks, "priority", cfg.Priority, "version", agent.Version)
	ag := agent.New(cfg, cred, nil, log)
	// SIGHUP re-reads the config's runtime allowlist and re-runs the
	// runtime preflight. Other settings need a restart.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				nc, err := agent.LoadConfig(path)
				if err != nil {
					log.Warn("config reload failed; keeping the current runtimes", "err", err)
					continue
				}
				ag.ReloadRuntimes(nc.Runtimes)
				avail, unavail := ag.RuntimeStatus()
				log.Info("config reloaded (runtimes only)", "available", avail, "unavailable", len(unavail))
			}
		}
	}()
	return ag.Run(ctx)
}

func enrol(path string) error {
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		return err
	}
	if cfg.CredentialFile == "" {
		return errors.New("credential_file must be set in the config")
	}
	token := strings.TrimSpace(os.Getenv("UMM_ENROL_TOKEN"))
	if token == "" {
		fmt.Fprint(os.Stderr, "Enrolment token: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		token = strings.TrimSpace(line)
	}
	if token == "" {
		return errors.New("no enrolment token")
	}
	res, err := agent.Enrol(context.Background(), cfg, token)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.CredentialFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.CredentialFile, []byte(res.Credential+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Enrolled as %s (%s). Credential written to %s\n", cfg.Name, res.AgentID, cfg.CredentialFile)
	return nil
}

func snapshot(path string) error {
	opts := telemetry.Options{UserLoadScope: "console", OurUID: os.Getuid()}
	if cfg, err := agent.LoadConfig(path); err == nil {
		opts.WorkDir, opts.UserLoadScope = cfg.WorkDir, cfg.UserLoadScope
	}
	t, err := telemetry.NewDefault(opts).Collect(context.Background())
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(t)
}

// update runs one self-update cycle. It must run as root (its LaunchDaemon
// has no UserName): it replaces the root-owned agent binary.
func update(path, label, bin, stateDir string, jitter time.Duration, dryRun, verbose bool) error {
	lvl := slog.LevelInfo
	if verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})).With("component", "updater")
	if runtime.GOOS == "darwin" && os.Geteuid() != 0 && !dryRun {
		return errors.New("update must run as root (it is started by its own LaunchDaemon)")
	}
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		return err
	}
	if !cfg.Update.UpdatesEnabled() {
		log.Info("updates disabled in agent config (update.enabled: false)")
		return nil
	}
	cred, err := cfg.LoadCredential()
	if err != nil || cred == "" {
		log.Info("agent not enrolled yet; nothing to do")
		return nil
	}
	pub, err := selfupdate.ReleasePublicKey()
	if cfg.Update.PublicKey != "" {
		pub, err = selfupdate.ParsePublicKey(cfg.Update.PublicKey)
	}
	if err != nil {
		return fmt.Errorf("release public key: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if jitter > 0 {
		d := rand.N(jitter)
		log.Debug("jitter", "sleep", d)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d):
		}
	}
	unlock, err := selfupdate.Lock(stateDir)
	if err != nil {
		return err
	}
	defer unlock()

	router := &selfupdate.HTTPRouter{Server: cfg.Server, Credential: cred}
	var sources []selfupdate.Source
	switch cfg.Update.Source {
	case "router":
		sources = []selfupdate.Source{router.RelaySource()}
	case "direct":
		sources = []selfupdate.Source{selfupdate.DirectSource(cfg.Update.ReleaseURL, nil)}
	default:
		sources = []selfupdate.Source{router.RelaySource(), selfupdate.DirectSource(cfg.Update.ReleaseURL, nil)}
	}
	u := &selfupdate.Updater{
		Router: router, Sources: sources, PublicKey: pub,
		Service: selfupdate.Launchctl{Label: label, Plist: "/Library/LaunchDaemons/" + label + ".plist"},
		BinPath: bin, StateDir: stateDir, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		VerifyTimeout: cfg.Update.VerifyTimeout, DryRun: dryRun, Log: log,
	}
	res, err := u.RunOnce(ctx)
	log.Info("update cycle done", "action", res.Action, "from", res.From, "to", res.To, "message", res.Message)
	return err
}
