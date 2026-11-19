package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/agent"
	"github.com/alee-ibrahim/use-my-mac/internal/hostprep"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
)

// runtimesSync is the root helper: its LaunchDaemon runs it whenever the
// agent writes a runtime request, and the updater runs it every cycle.
func runtimesSync(path, stateDir string, verbose bool) error {
	lvl := slog.LevelInfo
	if verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})).With("component", "runtimes")
	if runtime.GOOS == "darwin" && os.Geteuid() != 0 {
		return errors.New("runtimes-sync must run as root (its LaunchDaemon starts it)")
	}
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		return err
	}
	return syncRuntimes(context.Background(), cfg, stateDir, log)
}

// syncRuntimes runs one root helper pass, one at a time.
func syncRuntimes(ctx context.Context, cfg *agent.Config, stateDir string, log *slog.Logger) error {
	if !cfg.ManagedRuntimes.On() {
		return nil
	}
	// Another pass (launchd trigger or the updater) may be running: wait
	// for it, then do our own (each pass re-reads the request file).
	var unlock func()
	var err error
	for deadline := time.Now().Add(2 * time.Minute); ; {
		if unlock, err = selfupdate.Lock(filepath.Join(stateDir, "runtimes")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Second)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	return agent.SyncRuntimes(ctx, cfg, log)
}

// helperPlist renders the root helper's LaunchDaemon. It watches the
// request file and its directory (the agent replaces the file by rename).
func helperPlist(label, bin, cfgPath, requestFile string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  use-my-mac managed runtimes helper (written by umm-agent prepare). Runs as
  root when the agent asks for a runtime: verifies the signed package and
  installs it read-only for the agent. No UI.
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + esc(label+".runtimes") + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + esc(bin) + `</string>
    <string>runtimes-sync</string>
    <string>-config</string>
    <string>` + esc(cfgPath) + `</string>
  </array>
  <key>WatchPaths</key>
  <array>
    <string>` + esc(requestFile) + `</string>
    <string>` + esc(filepath.Dir(requestFile)) + `</string>
  </array>
  <key>ThrottleInterval</key>
  <integer>5</integer>
  <key>LowPriorityIO</key>
  <true/>
  <key>Nice</key>
  <integer>10</integer>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>StandardOutPath</key>
  <string>/usr/local/var/log/umm-runtimes.log</string>
  <key>StandardErrorPath</key>
  <string>/usr/local/var/log/umm-runtimes.log</string>
</dict>
</plist>
`)
}

// ensureHelperDaemon installs, refreshes or removes the root helper's
// LaunchDaemon (macOS, root). It is idempotent.
func ensureHelperDaemon(cfg *agent.Config, label, cfgPath string, log *slog.Logger) {
	if runtime.GOOS != "darwin" || os.Geteuid() != 0 {
		return
	}
	plist := "/Library/LaunchDaemons/" + label + ".runtimes.plist"
	svc := "system/" + label + ".runtimes"
	launchctl := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "/bin/launchctl", args...).Run()
	}
	if !cfg.ManagedRuntimes.On() {
		if _, err := os.Stat(plist); err == nil {
			_ = launchctl("bootout", svc)
			_ = os.Remove(plist)
			log.Info("managed runtimes off: removed the runtimes helper daemon")
		}
		return
	}
	bin, err := os.Executable()
	if err != nil {
		log.Warn("runtimes helper daemon", "err", err)
		return
	}
	if p, err := filepath.EvalSymlinks(bin); err == nil {
		bin = p
	}
	// The request file exists from the start, owned by the agent's user.
	rf := cfg.ManagedRuntimes.RequestFile
	if _, err := os.Lstat(rf); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(rf, []byte("{}\n"), 0o644); err == nil {
			if uid, gid := hostprep.Owner(filepath.Dir(rf)); uid > 0 {
				_ = os.Chown(rf, uid, gid)
			}
		}
	}
	want := helperPlist(label, bin, cfgPath, rf)
	cur, _ := os.ReadFile(plist)
	loaded := launchctl("print", svc) == nil
	if bytes.Equal(cur, want) && loaded {
		return
	}
	if err := os.WriteFile(plist+".tmp", want, 0o644); err != nil {
		log.Warn("runtimes helper daemon", "err", err)
		return
	}
	_ = os.Chown(plist+".tmp", 0, 0)
	if err := os.Rename(plist+".tmp", plist); err != nil {
		log.Warn("runtimes helper daemon", "err", err)
		return
	}
	_ = launchctl("bootout", svc)
	for i := 0; i < 5; i++ {
		if err = launchctl("bootstrap", "system", plist); err == nil {
			log.Info("runtimes helper daemon loaded", "label", label+".runtimes")
			return
		}
		time.Sleep(time.Second)
	}
	log.Warn("could not load the runtimes helper daemon", "err", err)
}
