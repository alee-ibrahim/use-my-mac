package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/fleet"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

const fleetUsage = `umm fleet — install and manage the agent across Macs over SSH (admin workstation)

Usage:
  umm fleet bootstrap  [flags] [HOST...]   one-time: install sudoers + scripts (needs sudo password)
  umm fleet install    [flags] [HOST...]   copy bundle, run install, enrol, verify
  umm fleet upgrade    [flags] [HOST...]   replace the agent binary and restart
  umm fleet uninstall  [flags] [HOST...]   remove the agent (add -purge to remove all state)
  umm fleet status     [flags] [HOST...]   show agent version / install / telemetry

Flags:
  -inventory FILE            inventory YAML (default ./fleet.yaml)
  -bundle DIR                dir containing umm-agent (overrides inventory)
  -router URL                router URL (overrides inventory / UMM_SERVER)
  -ssh-key PATH              SSH private key for all hosts (else per-host / ssh-agent)
  -known-hosts PATH          known_hosts file (default ~/.ssh/known_hosts)
  -accept-new-host-keys      trust unknown host keys on first use (TOFU)
  -concurrency N             max hosts in parallel (default 5)
  -sudo-password-file PATH   file with the admin sudo password (bootstrap)
  -sudo-password-env NAME    env var NAME holding the sudo password (bootstrap)
  -admin-key-file PATH       file with the router admin API key (install/status)
  -admin-key-env NAME        env var NAME holding the router admin API key
  -ttl-hours N               enrolment token lifetime (default 24)
  -purge                     uninstall: also remove config, credential, user, sudoers

Secrets are never passed on the command line. The SSH key is a path (or ssh-agent);
the sudo password and admin key come from a file, a named env var, or a no-echo prompt.
`

func fleetCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" || args[0] == "--help" {
		fmt.Print(fleetUsage)
		if len(args) == 0 {
			return errors.New("usage: umm fleet <bootstrap|install|upgrade|uninstall|status>")
		}
		return nil
	}
	action := fleet.Action(args[0])
	switch action {
	case fleet.ActionBootstrap, fleet.ActionInstall, fleet.ActionUpgrade, fleet.ActionUninstall, fleet.ActionStatus:
	default:
		fmt.Print(fleetUsage)
		return fmt.Errorf("unknown fleet action %q", args[0])
	}

	fs := flag.NewFlagSet("fleet "+args[0], flag.ExitOnError)
	invPath := fs.String("inventory", "fleet.yaml", "inventory YAML")
	bundle := fs.String("bundle", "", "dir with umm-agent")
	router := fs.String("router", "", "router URL")
	sshKey := fs.String("ssh-key", "", "SSH private key path")
	knownHosts := fs.String("known-hosts", "", "known_hosts path")
	acceptNew := fs.Bool("accept-new-host-keys", false, "trust unknown host keys on first use")
	conc := fs.Int("concurrency", 5, "max hosts in parallel")
	sudoPwFile := fs.String("sudo-password-file", "", "file with the admin sudo password")
	sudoPwEnv := fs.String("sudo-password-env", "", "env var NAME with the sudo password")
	adminKeyFile := fs.String("admin-key-file", "", "file with the router admin API key")
	adminKeyEnv := fs.String("admin-key-env", "", "env var NAME with the router admin API key")
	ttl := fs.Int("ttl-hours", 24, "enrolment token lifetime")
	purge := fs.Bool("purge", false, "uninstall: also remove config/credential/user/sudoers")
	only := parseFlags(fs, args[1:])

	inv, err := fleet.LoadInventory(*invPath)
	if err != nil {
		return err
	}
	hosts, err := inv.Resolve(only...)
	if err != nil {
		return err
	}

	// SSH signers: the -ssh-key flag (for all hosts) or each host's key, plus
	// ssh-agent. Never the key material on the command line — only paths.
	var keyPaths []string
	if *sshKey != "" {
		keyPaths = []string{*sshKey}
	} else {
		for _, h := range hosts {
			if h.SSHKey != "" {
				keyPaths = append(keyPaths, h.SSHKey)
			}
		}
	}
	signers, err := fleet.LoadSigners(keyPaths...)
	if err != nil {
		return err
	}
	hostKey, _, err := fleet.HostKeyChecker(*knownHosts, *acceptNew)
	if err != nil {
		return err
	}

	routerURL := firstNonEmpty(*router, inv.Router, os.Getenv("UMM_SERVER"))
	bundleDir := firstNonEmpty(*bundle, inv.Bundle)

	opt := fleet.Options{
		Router:        routerURL,
		Bundle:        bundleDir,
		EnrolTTLHours: *ttl,
		Purge:         *purge,
		Concurrency:   *conc,
		SSH:           fleet.SSHOptions{Signers: signers, HostKey: hostKey, Timeout: 20 * time.Second},
	}

	// Sudo password is only needed for bootstrap.
	if action == fleet.ActionBootstrap {
		sec, err := loadSecret(*sudoPwFile, *sudoPwEnv, "Admin sudo password: ")
		if err != nil {
			return fmt.Errorf("sudo password: %w", err)
		}
		opt.SudoPassword = sec
	}

	// Router client (admin) for minting enrolment tokens and verifying
	// telemetry. Optional for bootstrap/upgrade/uninstall.
	if action == fleet.ActionInstall || action == fleet.ActionStatus {
		rc, err := fleetRouterClient(routerURL, *adminKeyFile, *adminKeyEnv)
		if err != nil {
			if action == fleet.ActionInstall {
				fmt.Fprintf(os.Stderr, "warning: no router admin key (%v); installing without enrolment\n", err)
			}
		} else {
			opt.Router_ = rc
		}
	}

	runner := fleet.NewRunner(opt)
	results := runner.Run(ctx, hosts, action)
	return printFleetResults(results)
}

// loadSecret resolves a sudo/admin secret from a file, a named env var, or a
// no-echo prompt. The value never appears in argv.
func loadSecret(file, env, prompt string) (fleet.Secret, error) {
	switch {
	case file != "":
		return fleet.SecretSource{File: file}.Load()
	case env != "":
		return fleet.SecretSource{Env: env}.Load()
	default:
		return fleet.SecretSource{Prompt: true, PromptLabel: prompt}.Load()
	}
}

// fleetRouterClient builds the admin client. The key comes from a file, a named
// env var, or the usual UMM_API_KEY sources — never the command line.
func fleetRouterClient(router, keyFile, keyEnv string) (fleet.RouterAPI, error) {
	if router == "" {
		return nil, errors.New("no router URL")
	}
	var key string
	switch {
	case keyFile != "":
		sec, err := fleet.SecretSource{File: keyFile}.Load()
		if err != nil {
			return nil, err
		}
		key = string(sec.Bytes())
	case keyEnv != "":
		sec, err := fleet.SecretSource{Env: keyEnv}.Load()
		if err != nil {
			return nil, err
		}
		key = string(sec.Bytes())
	default:
		k, err := apiKey()
		if err != nil {
			return nil, err
		}
		key = k
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("empty admin key")
	}
	return client.New(router, key), nil
}

func printFleetResults(results []fleet.HostResult) error {
	w := tw()
	fmt.Fprintln(w, "HOST\tACTION\tRESULT\tTIME\tDETAIL")
	fails := 0
	for _, r := range results {
		detail := r.Message
		if r.Err != nil {
			if detail != "" {
				detail += ": "
			}
			detail += r.Err.Error()
			fails++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Host, r.Action, r.Status, r.Duration.Round(time.Millisecond), detail)
	}
	w.Flush()
	if fails > 0 {
		return fmt.Errorf("%d/%d host(s) failed", fails, len(results))
	}
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
