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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alee-ibrahim/use-my-mac/internal/agent"
	"github.com/alee-ibrahim/use-my-mac/internal/agent/telemetry"
)

const usage = `umm-agent — use-my-mac worker

Usage:
  umm-agent run       [-config FILE]   run the agent (launchd does this)
  umm-agent enrol     [-config FILE]   exchange a one-time enrolment token for a credential;
                                       the token is read from UMM_ENROL_TOKEN or stdin
  umm-agent telemetry [-config FILE]   print one telemetry snapshot as JSON (diagnostics)
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
	return agent.New(cfg, cred, nil, log).Run(ctx)
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
