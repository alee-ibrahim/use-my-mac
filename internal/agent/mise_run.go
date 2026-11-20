package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// miseRun is a prepared mise task: the argv prefix and environment to run
// its program with.
type miseRun struct {
	prefix []string // <mise> exec -- <program>
	args   []string // the task's arguments after the program
	env    []string
	prog   string // the program, for the progress parser
}

// prepareMise sets up a mise task: it puts the task's mise.toml and
// mise.lock where mise will read them, has mise report what they ask for
// and checks that against this Mac's settings, installs the tools into the
// task's work dir, and resolves the program to run. mise is the runtime's
// argv prefix (the admin's mise binary). On failure it returns the update
// to report: a task error, unless the Mac is at fault.
func (a *Agent) prepareMise(ctx context.Context, r *run, deadline time.Time, workdir string, mise, args, env []string,
	logs io.Writer, upd api.TaskUpdate) (*miseRun, *api.TaskUpdate) {
	mc := a.cfg.Mise
	fail := func(msg string) (*miseRun, *api.TaskUpdate) {
		upd.Outcome, upd.Error = api.OutcomeFailed, "mise: "+msg
		return nil, &upd
	}
	macFault := func(msg string) (*miseRun, *api.TaskUpdate) {
		upd.Outcome, upd.Error, upd.MacFault = api.OutcomeFailed, "mise: "+msg, "mise: "+msg
		return nil, &upd
	}
	prog, err := CheckMiseCommand(args, mc.Commands)
	if err != nil {
		return fail(err.Error())
	}

	// The config goes next to where the program runs (the work dir), so
	// `mise exec` finds it. MISE_CEILING_PATHS keeps mise from looking
	// further up, and global and system config point at nothing.
	config := filepath.Join(workdir, MiseConfigInput)
	if err := copyInput(workdir, MiseConfigInput, config); err != nil {
		return fail(err.Error())
	}
	if err := copyInput(workdir, MiseLockInput, filepath.Join(workdir, MiseLockInput)); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return fail(err.Error())
	} // a missing lockfile is mise's to refuse (MISE_LOCKED)
	p := newMisePaths(workdir)
	for _, d := range []string{p.Data, p.Cache} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return macFault(err.Error())
		}
	}
	if mc.Locked() {
		// Only downloads mise checks against a locked checksum are kept
		// and reused, and it checks them on every install, so the
		// download cache can be shared. Installed tools never are.
		shared := filepath.Join(mc.CacheDir, "downloads-cas")
		if err := os.MkdirAll(shared, 0o700); err != nil {
			return macFault("download cache: " + err.Error())
		}
		if err := os.Symlink(shared, filepath.Join(p.Cache, "downloads-cas")); err != nil {
			return macFault("download cache: " + err.Error())
		}
	}
	menv := MiseEnv(p, filepath.Dir(workdir), mc.Locked(), mc.MaxCacheBytes)
	// mise gets the agent's task_env, not the task's own variables.
	base, err := TaskEnv(workdir, "", 0, a.cfg.TaskEnv, nil)
	if err != nil {
		return fail(err.Error())
	}
	menvList := withEnv(base, menv)
	query := func(arg ...string) ([]byte, error) {
		return miseOutput(ctx, mise, workdir, menvList, logs, arg...)
	}

	// What does the config ask for? mise reads it (MISE_SAFE: nothing in
	// it runs) and says; the agent only checks its answer.
	// mise must have loaded the task's mise.toml and nothing else (no
	// file left in the work volume's root by an earlier task, no global
	// or system config).
	out, err := query("config", "ls", "--json")
	if err == nil {
		err = CheckMiseConfigFiles(out, config)
	}
	if err != nil {
		return fail(err.Error())
	}
	out, err = query("config", "explain", "--file", config, "--json")
	var listed []string
	if err == nil {
		listed, err = CheckMiseKeys(out)
	}
	if err != nil {
		return fail(err.Error())
	}
	if out, err = query("ls", "--current", "--json"); err != nil {
		return fail(err.Error())
	}
	tools, err := MiseTools(out, config, listed)
	if err != nil {
		return fail(err.Error())
	}
	// Each tool's backend, as mise resolves it, must be allowed here.
	backends := map[string]string{}
	for _, t := range tools {
		out, err := query("tool", t, "--json")
		var b string
		if err == nil {
			b, err = MiseBackend(out)
		}
		if err != nil {
			return fail(t + ": " + err.Error())
		}
		if !MiseToolAllowed(mc.AllowTools, b) {
			return fail(fmt.Sprintf("tool %s (%s) is not allowed on this Mac (mise.allow_tools)", t, b))
		}
		backends[t] = b
	}
	tok, err := a.miseGitHubToken(ctx)
	if err != nil {
		return macFault(err.Error())
	}
	// The token goes only to installs of tools whose backend just downloads
	// release files; tools whose backend runs install-time code (package
	// managers, source builds) install in a separate step without it.
	var plain, withToken []string
	for _, t := range tools {
		if tok != "" && MiseTokenBackend(backends[t]) {
			withToken = append(withToken, t)
		} else {
			plain = append(plain, t)
		}
	}
	ictx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	start := time.Now()
	install := func(names []string, env []string) *api.TaskUpdate {
		fmt.Fprintf(logs, "[agent] mise: installing %s\n", strings.Join(names, ", "))
		full := &diskFullTap{}
		tail := &tailBuffer{max: 64 << 10}
		args := []string{"install"}
		if len(names) < len(tools) {
			args = append(args, names...)
		}
		argv := BuildArgv(a.goos, launchPriority(a.cfg.Priority, a.currentQoS()), mise, args)
		cmd := exec.CommandContext(ictx, argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = workdir, env
		// One writer for both, so exec.Cmd copies them in one goroutine.
		w := io.MultiWriter(logs, full, tail)
		cmd.Stdout, cmd.Stderr = w, w
		cmd.WaitDelay = a.cfg.StopGrace
		setupProc(cmd)
		if err := cmd.Start(); err != nil {
			if why := execFailureReason(mise[0], err, 0); why != "" {
				upd.RuntimeUnavailable = why
				a.markExecFault(MiseRuntime, why)
			}
			_, u := fail("start: " + err.Error())
			return u
		}
		a.setPgid(r, cmd.Process.Pid) // its CPU is ours, not the Mac user's
		werr := cmd.Wait()
		killGroup(cmd.Process.Pid)
		a.setPgid(r, 0)
		switch {
		case ctx.Err() != nil:
			u := a.stopped(ctx, upd, "")
			return &u
		case errors.Is(ictx.Err(), context.DeadlineExceeded):
			u := upd
			u.Outcome, u.Error = api.OutcomeTimeout, "mise: installing the tools did not finish within the task's timeout"
			return &u
		case werr != nil && full.seen:
			_, u := macFault("install ran out of disk space (" + werr.Error() + ")")
			return u
		case werr != nil && MiseInstallFault(tail.String()) != "":
			// The Mac's network, not the task: try another Mac.
			_, u := macFault(MiseInstallFault(tail.String()) + " (" + werr.Error() + "); mise's output is in the task log")
			return u
		case werr != nil:
			if why := CheckProgram(mise[0]); why != "" {
				upd.RuntimeUnavailable = why
				a.markExecFault(MiseRuntime, why)
			}
			_, u := fail("installing the tools failed (" + werr.Error() + "); mise's output is in the task log")
			return u
		}
		return nil
	}
	if len(plain) > 0 {
		if u := install(plain, menvList); u != nil {
			return nil, u
		}
	}
	if len(withToken) > 0 {
		// Never the task's program, never logged.
		if u := install(withToken, withEnv(menvList, map[string]string{"MISE_GITHUB_TOKEN": tok})); u != nil {
			return nil, u
		}
	}
	fmt.Fprintf(logs, "[agent] mise: tools ready in %s\n", time.Since(start).Round(100*time.Millisecond))

	// The program must come from one of the installed tools, never from
	// the system (/bin/sh, a Homebrew program).
	out, err = query("which", prog)
	path := strings.TrimSpace(string(out))
	if err != nil {
		return fail(fmt.Sprintf("no installed tool provides %s (%v)", prog, err))
	}
	if !insideDir(path, filepath.Join(p.Data, "installs")) {
		return fail(fmt.Sprintf("program %s resolves to %q, outside the installed tools", prog, path))
	}
	// Follow links too (bin/node -> /bin/sh). The program still runs by its
	// own name: some tools behave by the name they are called with.
	if _, err := miseProgramInside(path, filepath.Join(p.Data, "installs")); err != nil {
		return fail("program " + prog + ": " + err.Error())
	}
	if why := CheckProgram(path); why != "" {
		return fail(why)
	}
	return &miseRun{
		prefix: append(append([]string(nil), mise...), "exec", "--", path),
		args:   args[1:],
		env:    withEnv(env, menv),
		prog:   path,
	}, nil
}

// miseOutput runs a quick mise query and returns its standard output, or
// an error with the first line mise printed on standard error. What mise
// prints on standard error (warnings) also goes to the task log.
func miseOutput(ctx context.Context, mise []string, dir string, env []string, logs io.Writer, arg ...string) ([]byte, error) {
	qctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(qctx, mise[0], append(append([]string(nil), mise[1:]...), arg...)...)
	var out, errb bytes.Buffer
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, &out, &errb
	err := cmd.Run()
	logs.Write(errb.Bytes())
	if err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(errb.String()), "\n")
		return nil, fmt.Errorf("mise %s: %v: %s", arg[0], err, strings.TrimPrefix(msg, "mise ERROR "))
	}
	return out.Bytes(), nil
}

// copyInput copies one of the task's inputs (a small regular file) to dst.
func copyInput(workdir, name, dst string) error {
	p := filepath.Join(workdir, "in", name)
	st, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the task has no input named %s: %w", name, os.ErrNotExist)
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > maxMiseFile {
		return fmt.Errorf("input %s must be a regular file of at most %d KiB", name, maxMiseFile>>10)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func (a *Agent) setPgid(r *run, pgid int) {
	a.mu.Lock()
	r.pgid = pgid
	a.mu.Unlock()
}

// miseGitHubToken returns the admin's GitHub token for installs ("" when
// none is configured). Errors are the Mac's (its settings or secrets
// manager), and never include the token or the command's output.
func (a *Agent) miseGitHubToken(ctx context.Context) (string, error) {
	mc := a.cfg.Mise
	switch {
	case mc.GitHubTokenFile != "":
		st, err := os.Stat(mc.GitHubTokenFile)
		if err != nil {
			return "", fmt.Errorf("github_token_file: %w", err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("github_token_file %s must not be readable by group or others (chmod 600)", mc.GitHubTokenFile)
		}
		b, err := os.ReadFile(mc.GitHubTokenFile)
		if err != nil {
			return "", fmt.Errorf("github_token_file: %w", err)
		}
		tok, err := ParseGitHubToken(b)
		if err != nil {
			return "", fmt.Errorf("github_token_file: %w", err)
		}
		return tok, nil
	case len(mc.GitHubTokenCommand) > 0:
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, mc.GitHubTokenCommand[0], mc.GitHubTokenCommand[1:]...)
		// Not the agent's environment: only what a CLI needs to find its
		// own configuration.
		cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8"}
		if home, err := os.UserHomeDir(); err == nil {
			cmd.Env = append(cmd.Env, "HOME="+home)
		}
		var out bytes.Buffer
		cmd.Stdout = &limitWriter{w: &out, n: 64 << 10}
		// stderr is dropped: a secrets manager may echo what it fetched.
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("github_token_command %s failed: %v", filepath.Base(mc.GitHubTokenCommand[0]), err)
		}
		tok, err := ParseGitHubToken(out.Bytes())
		if err != nil {
			return "", fmt.Errorf("github_token_command: %w", err)
		}
		return tok, nil
	}
	return "", nil
}

// limitWriter keeps the first n bytes and discards the rest.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.n -= k
		if _, err := l.w.Write(p[:k]); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
