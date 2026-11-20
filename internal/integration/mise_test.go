package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/agent"
	"github.com/alee-ibrahim/use-my-mac/internal/engine"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

// fakeMise stands in for mise where it is not installed (CI). It reads the
// tool names under [tools] in mise.toml and answers the agent's queries
// for them: `config ls` and `config explain` (reporting [env] when the file
// has one), `ls`, `tool` (backend: the name if it has one, else
// aqua:example/NAME), `install` (puts a program named after each tool,
// plus a link "shlink" to /bin/sh, into $MISE_DATA_DIR/installs; fails for
// a config that mentions "broken", or with $FAKE_MISE_FAIL's text; records
// each install and whether it got a GitHub token), `which` and `exec`.
// The programs say whether they can see a token.
const fakeMise = `#!/bin/sh
tools() { sed -n '/^\[tools\]/,/^\[[a-z]/p' mise.toml | sed -n 's/^"\{0,1\}\([A-Za-z0-9:\/._-]*\)"\{0,1\} *=.*/\1/p'; }
case "$1" in
--version) echo "2026.11.9 linux-x64 (2026-11-16)" ;;
config)
	if [ "$2" = ls ]; then echo '[{"path":"'"$PWD"'/mise.toml"}]'; exit 0; fi
	if grep -q '^\[env\]' "$4"; then echo '[{"key":"env","path":["env"]},{"key":"env.X","path":["env","X"]}]'; exit 0; fi
	printf '[{"key":"tools","path":["tools"]}'
	for t in $(tools); do printf ',{"key":"tools.%s","path":["tools","%s"]}' "$t" "$t"; done
	echo ']'
	;;
ls)
	sep=; printf '{'
	for t in $(tools); do
		printf '%s"%s":[{"requested_version":"1.0.0","source":{"type":"mise.toml","path":"%s/mise.toml"}}]' "$sep" "$t" "$PWD"; sep=,
	done
	echo '}'
	;;
tool)
	case "$2" in *:*) b=$2 ;; *) b=aqua:example/$2 ;; esac
	echo '{"backend":"'"$b"'"}'
	;;
install)
	shift; want=$*; [ -z "$want" ] && want=$(tools)
	echo "fake-mise install" $want
	env | grep -E '^MISE_(SAFE|LOCKED|EXEC_AUTO_INSTALL|SYSTEM_PACKAGES_SUDO)=' | sort
	[ -n "$MISE_GITHUB_TOKEN" ] && echo "github token: set"
	echo $want "token=${MISE_GITHUB_TOKEN:+set}" >> "$FAKE_MISE_RECORD/installs"
	printf %s "$MISE_GITHUB_TOKEN" > "$FAKE_MISE_RECORD/install-token"
	if [ -n "$FAKE_MISE_FAIL" ]; then echo "mise ERROR $FAKE_MISE_FAIL" >&2; exit 1; fi
	if grep -q broken mise.toml; then echo "download failed: 404" >&2; exit 1; fi
	for t in $want; do
		n=${t##*[:/]}
		bin="$MISE_DATA_DIR/installs/$n/1.0.0/bin"
		mkdir -p "$bin"
		printf '#!/bin/sh\n[ "$1" = exit127 ] && exit 127\necho "hello from the toolchain; task sees token: ${MISE_GITHUB_TOKEN:-none}${GITHUB_TOKEN:-}"\nprintf %%s "$1" > "$2"\n' > "$bin/$n"
		chmod 755 "$bin/$n"
		ln -sf /bin/sh "$bin/shlink"
	done
	;;
which)
	for p in "$MISE_DATA_DIR"/installs/*/1.0.0/bin/"$2"; do
		if [ -e "$p" ]; then echo "$p"; exit 0; fi
	done
	echo "mise ERROR $2 is not a mise bin" >&2; exit 1
	;;
exec)
	shift; [ "$1" = "--" ] && shift
	exec "$@"
	;;
*) exit 2 ;;
esac
`

const helloToml = "[tools]\nhello = \"1.0.0\"\n"

// blob uploads an input as the app that submits the jobs (blobs belong to
// their uploader).
func (e *env) blob(key, data string) {
	e.t.Helper()
	if err := e.app.UploadBlob(e.ctx, key, strings.NewReader(data), int64(len(data))); err != nil {
		e.t.Fatal(err)
	}
}

// miseInputs uploads a mise.toml (and a mise.lock unless lock is "") and
// returns them as task inputs.
func (e *env) miseInputs(key, toml, lock string) []api.Input {
	e.blob(key+"/mise.toml", toml)
	in := []api.Input{{Name: "mise.toml", Blob: key + "/mise.toml"}}
	if lock != "" {
		e.blob(key+"/mise.lock", lock)
		in = append(in, api.Input{Name: "mise.lock", Blob: key + "/mise.lock"})
	}
	return in
}

func (e *env) miseMac(name, miseBin string, mut func(*agent.Config)) (*fakeMac, string) {
	cache := filepath.Join(e.t.TempDir(), "mise-cache")
	f := e.macCfg(name, 1, quietTel(), nil, func(c *agent.Config) {
		c.Mise = agent.MiseConfig{Enabled: true, Binary: miseBin, CacheDir: cache}
		miseHostChecksOff(c)
		if mut != nil {
			mut(c)
		}
	})
	waitFor(e.t, name+" to offer the mise runtime", 5*time.Second, func() bool {
		return slices.Contains(macByID(e, f.id).Runtimes, agent.MiseRuntime)
	})
	return f, cache
}

// runOne submits a one-task job and waits for it to end.
func (e *env) runOne(ts api.TaskSpec) api.Task {
	e.t.Helper()
	zero := 0
	ts.Key, ts.MaxRetries, ts.Runtime = "t", &zero, agent.MiseRuntime
	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Name: "mise", Tasks: []api.TaskSpec{ts}})
	if err != nil {
		e.t.Fatal(err)
	}
	final, err := e.app.WaitJob(e.ctx, job.ID, 100*time.Millisecond, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return final.Tasks[0]
}

func (e *env) taskError(id string) string {
	e.t.Helper()
	d, err := e.app.Task(e.ctx, id)
	if err != nil || len(d.Attempts) == 0 {
		e.t.Fatalf("task %s: %v %+v", id, err, d)
	}
	return d.Attempts[len(d.Attempts)-1].Error
}

func (e *env) logs(id string) string {
	l, _ := e.app.Logs(e.ctx, id, 0)
	return l
}

// A Mac with mise enabled advertises the runtime "mise" and runs a task's
// pinned toolchain: mise reports what the task's mise.toml asks for, the
// agent installs it into the task's work dir with locked, safe-mode
// settings and runs a program the tools provide. A Mac without mise never
// gets the task. A config with more than [tools], a program from outside
// the tools, MISE_* or token variables in the task's env, a failed install
// and the program's own exit 127 are task errors, not runtime faults. The admin's GitHub token (from a file or a
// command) reaches `mise install` only: not the task's program, not the
// logs.
func TestMiseToolchain(t *testing.T) {
	skipNoShell(t)
	// These are task errors on purpose, several in a row on one Mac, which
	// would quarantine it (quarantine_after) like any failing tasks.
	e := newEnvCfg(t, func(c *engine.Config) { c.QuarantineAfter = 0 })
	dir := t.TempDir()
	bin := installFakeMise(t, fakeMise)
	const token = "github_pat_test0000000000secret"
	tokenFile := filepath.Join(dir, "github-token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := t.TempDir()
	mac, cache := e.miseMac("mise-mac", bin, func(c *agent.Config) {
		c.Mise.AllowTools = []string{"aqua:*", "npm:*"}
		c.Mise.GitHubTokenFile = tokenFile
		c.TaskEnv["FAKE_MISE_RECORD"] = record
	})
	plain := e.mac("plain-mac", 1, quietTel())
	if slices.Contains(macByID(e, plain.id).Runtimes, agent.MiseRuntime) {
		t.Fatal("plain-mac must not offer mise")
	}
	lock := "lockfile_version = 4\n" // the fake does not read it

	task := e.runOne(api.TaskSpec{Args: []string{"hello", "pinned", "{out:o.txt}"},
		Inputs: e.miseInputs("ok", helloToml, lock), Outputs: []api.Output{{Name: "o.txt"}}})
	if task.State != api.StateSucceeded || task.AgentID != mac.id {
		t.Fatalf("task %s on %s: %s\n%s", task.State, task.AgentID, e.taskError(task.ID), e.logs(task.ID))
	}
	var out bytes.Buffer
	if _, err := e.app.DownloadOutput(e.ctx, task.ID, "o.txt", &out); err != nil || out.String() != "pinned" {
		t.Fatalf("output %q, %v", out.String(), err)
	}
	logs := e.logs(task.ID)
	for _, want := range []string{"[agent] mise: installing hello", "fake-mise install", "MISE_SAFE=1", "MISE_LOCKED=1",
		"MISE_EXEC_AUTO_INSTALL=0", "MISE_SYSTEM_PACKAGES_SUDO=0", "github token: set", "[agent] mise: tools ready",
		"hello from the toolchain; task sees token: none\n"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, token) {
		t.Errorf("the token is in the task log:\n%s", logs)
	}
	if b, _ := os.ReadFile(filepath.Join(record, "install-token")); string(b) != token {
		t.Errorf("mise install got token %q", b)
	}
	if _, err := os.Stat(filepath.Join(cache, "downloads-cas")); err != nil {
		t.Errorf("shared download cache: %v", err)
	}

	fails := []struct {
		name, toml string
		args       []string
		env        map[string]string
		want       string
		installed  bool
	}{
		{"env in config", "[env]\nX = \"1\"\n" + helloToml, nil, nil, "env is not allowed", false},
		{"program outside the tools", helloToml, []string{"sh", "-c", "id"}, nil, "no installed tool provides sh", true},
		{"path as program", helloToml, []string{"/bin/sh", "-c", "id"}, nil, "bare name", false},
		{"MISE_ env", helloToml, nil, map[string]string{"MISE_ENV": "x"}, "may not set MISE_ENV", false},
		{"token in task env", helloToml, nil, map[string]string{"GITHUB_TOKEN": "x"}, "may not set GITHUB_TOKEN", false},
		{"install fails", "[tools]\nhello = \"1.0.0\" # broken\n", nil, nil, "installing the tools failed", true},
		{"link out of the tools", helloToml, []string{"shlink", "-c", "id"}, nil, "outside the installed tools", true},
		{"tool not allowed", "[tools]\n\"github:someone/tool\" = \"1.0.0\"\n", nil, nil,
			"github:someone/tool (github:someone/tool) is not allowed on this Mac", false},
		// The program's own exit 127 is the task's: mise is still there.
		{"program exits 127", helloToml, []string{"hello", "exit127"}, nil, "exit status 127", true},
	}
	for _, f := range fails {
		args := f.args
		if args == nil {
			args = []string{"hello", "x", "{out:o.txt}"}
		}
		task := e.runOne(api.TaskSpec{Args: args, Env: f.env, Inputs: e.miseInputs(strings.ReplaceAll(f.name, " ", "-"), f.toml, lock),
			Outputs: []api.Output{{Name: "o.txt", Optional: true}}})
		msg := e.taskError(task.ID)
		if task.State != api.StateFailed || !strings.Contains(msg, f.want) {
			t.Errorf("%s: %s %q, want a failure mentioning %q", f.name, task.State, msg, f.want)
		}
		if ran := strings.Contains(e.logs(task.ID), "fake-mise install"); ran != f.installed {
			t.Errorf("%s: mise install ran = %v", f.name, ran)
		}
	}
	if m := macByID(e, mac.id); !slices.Contains(m.Runtimes, agent.MiseRuntime) {
		t.Errorf("task errors must not make the mise runtime unavailable: %+v", m.UnavailableRuntimes)
	}

	// The token goes only to installs of tools whose backend just downloads
	// release files (aqua:, github:, ubi:), never to backends that run
	// install-time code (npm: here): they install in a separate step.
	mixed := e.runOne(api.TaskSpec{Args: []string{"hello", "x", "{out:o.txt}"},
		Inputs:  e.miseInputs("mixed", "[tools]\nhello = \"1.0.0\"\n\"npm:thing\" = \"1.0.0\"\n", lock),
		Outputs: []api.Output{{Name: "o.txt"}}})
	if mixed.State != api.StateSucceeded {
		t.Fatalf("mixed: %s %s\n%s", mixed.State, e.taskError(mixed.ID), e.logs(mixed.ID))
	}
	b, _ := os.ReadFile(filepath.Join(record, "installs"))
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); !slices.Contains(lines, "npm:thing token=") ||
		!slices.Contains(lines, "hello token=set") {
		t.Errorf("installs of the mixed task: %q", lines)
	}

	// A token from a command (a secrets manager's CLI): only its standard
	// output counts, it reaches the install only, and the command gets a
	// minimal environment, not the agent's.
	t.Setenv("UMM_TEST_AGENT_SECRET", "agent-only")
	record2 := t.TempDir()
	e.miseMac("cmd-mac", bin, func(c *agent.Config) {
		c.Mise.GitHubTokenCommand = []string{"/bin/sh", "-c",
			"env > " + filepath.Join(record2, "cmd-env") + "; echo noise-" + token + " >&2; echo " + token + "-cmd"}
		c.TaskEnv["FAKE_MISE_RECORD"] = record2
	})
	if err := e.admin.PatchMac(e.ctx, mac.id, api.MacPatch{Drain: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	task = e.runOne(api.TaskSpec{Args: []string{"hello", "x", "{out:o.txt}"}, Inputs: e.miseInputs("ok", helloToml, lock),
		Outputs: []api.Output{{Name: "o.txt"}}})
	logs = e.logs(task.ID)
	if task.State != api.StateSucceeded || strings.Contains(logs, token) || !strings.Contains(logs, "task sees token: none\n") {
		t.Fatalf("token command: %s %s\n%s", task.State, e.taskError(task.ID), logs)
	}
	if b, _ := os.ReadFile(filepath.Join(record2, "install-token")); string(b) != token+"-cmd" {
		t.Errorf("mise install got token %q", b)
	}
	b, _ = os.ReadFile(filepath.Join(record2, "cmd-env"))
	for _, kv := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains([]string{"PATH", "HOME", "LANG", "PWD", "SHLVL", "_", "OLDPWD"}, k) {
			t.Errorf("token command saw %s", k)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// realMise returns the mise binary to test with (UMM_TEST_MISE_BIN, else
// mise on PATH), or skips.
func realMise(t *testing.T) string {
	skipNoShell(t)
	bin := os.Getenv("UMM_TEST_MISE_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("mise"); err != nil {
			t.Skip("mise not installed (set UMM_TEST_MISE_BIN)")
		}
	}
	bin, _ = filepath.Abs(bin)
	return bin
}

// The real mise reads the task's config and refuses what the Mac does not
// allow, before installing anything. No network needed.
func TestMiseRealChecks(t *testing.T) {
	bin := realMise(t)
	// Task errors on purpose, many in a row on one Mac (see TestMiseToolchain).
	e := newEnvCfg(t, func(c *engine.Config) { c.QuarantineAfter = 0 })
	e.miseMac("real-mise", bin, nil)
	jq := "[tools]\n\"aqua:jqlang/jq\" = \"1.7.1\"\n"
	for _, c := range []struct{ name, toml, lock, want string }{
		{"env", "[env]\nX = \"{{ exec(command='id') }}\"\n" + jq, realJQLock, "env is not allowed"},
		{"tasks", jq + "[tasks.build]\nrun = \"id\"\n", realJQLock, "tasks is not allowed"},
		{"hooks", "[hooks]\nenter = \"id\"\n" + jq, realJQLock, "hooks is not allowed"},
		{"settings", "[settings]\nlocked = false\n" + jq, realJQLock, "settings is not allowed"},
		{"no tools", "min_version = \"2026.1.0\"\n", realJQLock, "[tools] is missing"},
		{"not toml", "[tools\n", realJQLock, "mise config"},
		{"no lockfile", jq, "", "cannot resolve aqua:jqlang/jq"},
		{"tool not in lockfile", "[tools]\n\"aqua:BurntSushi/ripgrep\" = \"14.1.1\"\n", realJQLock, "cannot resolve aqua:BurntSushi/ripgrep"},
	} {
		task := e.runOne(api.TaskSpec{Args: []string{"jq", "-n", "1"}, Inputs: e.miseInputs("c/"+strings.ReplaceAll(c.name, " ", "-"), c.toml, c.lock)})
		if msg := e.taskError(task.ID); task.State != api.StateFailed || !strings.Contains(msg, c.want) {
			t.Errorf("%s: %s %q, want a failure mentioning %q\n%s", c.name, task.State, msg, c.want, e.logs(task.ID))
		}
	}
}

// With UMM_TEST_MISE=1 the agent runs a real locked install of jq
// (downloaded from GitHub; needs network access to github.com and
// api.github.com, which mise's aqua backend queries even when locked) and
// `mise exec`. A second task gets the download from the shared cache; one
// a task tampered with is fetched again; a lockfile with the wrong
// checksum fails; a program from outside the tools never runs. The macOS
// CI job runs these as a non-admin user, like _umm on a Mac.
//
// UMM_TEST_MISE_GITHUB_TOKEN_FILE names a token file used as the Mac's
// mise.github_token_file: the install is authenticated, and the token must
// not reach the task's program or the logs. Without one, unauthenticated
// GitHub API calls are limited per IP (60 an hour), so the test skips if it
// hits that limit, except in GitHub Actions, where a token is required.

func TestMiseRealInstall(t *testing.T) {
	if os.Getenv("UMM_TEST_MISE") == "" {
		t.Skip("set UMM_TEST_MISE=1 to run a real mise install (downloads jq from github.com)")
	}
	bin := realMise(t)
	plat := map[string]string{"darwin/arm64": "macos-arm64", "linux/amd64": "linux-x64", "linux/arm64": "linux-arm64"}[runtime.GOOS+"/"+runtime.GOARCH]
	if plat == "" {
		t.Skipf("no jq checksum in the test lockfile for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	tokenFile, token := os.Getenv("UMM_TEST_MISE_GITHUB_TOKEN_FILE"), ""
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			t.Fatal(err)
		}
		token = strings.TrimSpace(string(b))
	} else if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal("set UMM_TEST_MISE_GITHUB_TOKEN_FILE: unauthenticated GitHub API calls from shared runners hit the rate limit")
	}
	e := newEnv(t)
	mac, cache := e.miseMac("real-mise", bin, func(c *agent.Config) {
		c.Mise.Commands = []string{"jq", "sh"}
		c.Mise.GitHubTokenFile = tokenFile
	})
	// An earlier task (all tasks are the same user) left config in the
	// work volume's root: it must not load for later tasks.
	for name, body := range map[string]string{"mise.toml": "[env]\nPLANTED = \"mise.toml\"\n",
		".tool-versions": "jq 1.6\n", ".mise.toml": "[env]\nPLANTED = \".mise.toml\"\n"} {
		if err := os.WriteFile(filepath.Join(mac.workDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var allLogs strings.Builder
	logs := func(task api.Task) string {
		l := e.logs(task.ID)
		allLogs.WriteString(l)
		return l
	}
	// run submits a one-task job and waits for it. A rate-limited install
	// is a Mac fault, so with this one Mac the task would wait for another:
	// without a token that skips the test, with one it fails it.
	run := func(ts api.TaskSpec) api.Task {
		t.Helper()
		zero := 0
		ts.Key, ts.MaxRetries, ts.Runtime = "t", &zero, agent.MiseRuntime
		job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Name: "mise", Tasks: []api.TaskSpec{ts}})
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			j, err := e.app.Job(e.ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			task := j.Tasks[0]
			switch task.State {
			case api.StateSucceeded, api.StateFailed, api.StateCancelled:
				return task
			}
			d, _ := e.app.Task(e.ctx, task.ID)
			for _, a := range d.Attempts {
				if a.Outcome == api.OutcomeUnavailable && strings.Contains(a.Error, "rate limit") {
					logs(task)
					if token == "" {
						t.Skip("GitHub's unauthenticated API rate limit was hit; set UMM_TEST_MISE_GITHUB_TOKEN_FILE")
					}
					t.Fatalf("rate-limited even with a token: %s", a.Error)
				}
			}
		}
		t.Fatal("the task did not finish")
		return api.Task{}
	}
	jq := "[tools]\n\"aqua:jqlang/jq\" = \"1.7.1\"\n"
	in := e.miseInputs("jq", jq, realJQLock)
	ts := api.TaskSpec{Args: []string{"jq", "-n", "-c", `{"tool": "jq", "args": $ARGS.positional}`, "--args", "pinned"},
		Inputs: in, TimeoutSeconds: 300}
	for i := 0; i < 2; i++ {
		task := run(ts)
		l := logs(task)
		if task.State != api.StateSucceeded || !strings.Contains(l, `{"tool":"jq","args":["pinned"]}`) {
			t.Fatalf("run %d: %s %s\n%s", i, task.State, e.taskError(task.ID), l)
		}
		t.Logf("run %d log:\n%s", i, l)
	}
	// The task's program does not see the token (jq can print its env), nor
	// anything from the planted config.
	task := run(api.TaskSpec{Args: []string{"jq", "-n", "-r",
		`"token in task env: " + (env.MISE_GITHUB_TOKEN // env.GITHUB_TOKEN // env.GITHUB_API_TOKEN // "none") + ", planted: " + (env.PLANTED // "none")`},
		Inputs: in, TimeoutSeconds: 300})
	if l := logs(task); task.State != api.StateSucceeded || !strings.Contains(l, "token in task env: none, planted: none\n") {
		t.Fatalf("env: %s %s\n%s", task.State, e.taskError(task.ID), l)
	}
	// The cache is shared by tasks running as the same user, so a task can
	// overwrite a download in it. mise re-checks each one against the
	// locked checksum before use, drops it and fetches it again.
	blobs, _ := filepath.Glob(filepath.Join(cache, "downloads-cas", "blobs", "*", "artifact"))
	if len(blobs) == 0 {
		t.Fatal("shared download cache is empty")
	}
	for _, b := range blobs {
		if err := os.WriteFile(b, []byte("#!/bin/sh\necho tampered\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	task = run(ts)
	if l := logs(task); task.State != api.StateSucceeded || strings.Contains(l, "tampered") ||
		!strings.Contains(l, `{"tool":"jq","args":["pinned"]}`) {
		t.Fatalf("tampered cache: %s %s\n%s", task.State, e.taskError(task.ID), l)
	}

	task = run(api.TaskSpec{Args: []string{"sh", "-c", "id"}, Inputs: in, TimeoutSeconds: 300})
	logs(task)
	if msg := e.taskError(task.ID); task.State != api.StateFailed || !strings.Contains(msg, "no installed tool provides sh") {
		t.Errorf("sh: %s %q", task.State, msg)
	}

	bad := realJQLock
	for _, sum := range []string{"4dd2d8a0661df0b22f1bb9a1f9830f06b6f3b8f7d91211a1ef5d7c4f06a8b4a5",
		"5942c9b0934e510ee61eb3e30273f1b3fe2590df93933a93d7c58b81d19c8ff5",
		"0bbe619e663e0de2c550be2fe0d240d076799d6f8a652b70fa04aea8a8362e8a"} {
		bad = strings.ReplaceAll(bad, sum, strings.Repeat("0", 64))
	}
	ts.Inputs = e.miseInputs("jq-bad", jq, bad)
	task = run(ts)
	if l := logs(task); task.State != api.StateFailed || !strings.Contains(l, "Checksum mismatch") {
		t.Fatalf("bad checksum: %s %s\n%s", task.State, e.taskError(task.ID), l)
	}
	if token != "" && strings.Contains(allLogs.String(), token) {
		t.Error("the GitHub token is in a task log")
	}
}

// realJQLock is `mise lock --platform linux-x64,linux-arm64,macos-arm64`
// for aqua:jqlang/jq 1.7.1 (mise 2026.11.9).
const realJQLock = `lockfile_version = 4

[[tools."aqua:jqlang/jq"]]
version = "1.7.1"
backend = "aqua:jqlang/jq"
specifiers = ["1.7.1"]

[tools."aqua:jqlang/jq"."platforms.linux-arm64"]
checksum = "sha256:4dd2d8a0661df0b22f1bb9a1f9830f06b6f3b8f7d91211a1ef5d7c4f06a8b4a5"
url = "https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-arm64"
url_api = "https://api.github.com/repos/jqlang/jq/releases/assets/140520982"

[tools."aqua:jqlang/jq"."platforms.linux-x64"]
checksum = "sha256:5942c9b0934e510ee61eb3e30273f1b3fe2590df93933a93d7c58b81d19c8ff5"
url = "https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64"
url_api = "https://api.github.com/repos/jqlang/jq/releases/assets/140520984"

[tools."aqua:jqlang/jq"."platforms.macos-arm64"]
checksum = "sha256:0bbe619e663e0de2c550be2fe0d240d076799d6f8a652b70fa04aea8a8362e8a"
url = "https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-macos-arm64"
url_api = "https://api.github.com/repos/jqlang/jq/releases/assets/140521010"
`

// scriptPackage builds a tar.gz with bin/<name> holding script.
func scriptPackage(name, script string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "bin/" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))})
	tw.Write([]byte(script))
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// The runtime name "mise" belongs to mise toolchains: a managed package
// providing a mise command is not offered or run as a plain runtime on a
// Mac without mise.enabled. With mise enabled and no mise.binary, that
// package is how the Mac gets mise (the default deployment), and tasks run
// through the protected path.
func TestMiseReservedName(t *testing.T) {
	skipNoShell(t)
	e := newEnv(t)
	s := newSigner(t)
	s.publish(e, "mise", "2026.11.9", scriptPackage("mise", fakeMise), "", false)
	record := t.TempDir()
	off, _ := e.managedMacCfg("off-mac", s, nil, nil)
	on, _ := e.managedMacCfg("on-mac", s, nil, func(c *agent.Config) {
		c.Mise = agent.MiseConfig{Enabled: true, CacheDir: filepath.Join(t.TempDir(), "cache")}
		c.TaskEnv["FAKE_MISE_RECORD"] = record
		miseHostChecksOff(c)
	})
	waitFor(t, "on-mac to offer mise", 5*time.Second, func() bool {
		m := macByID(e, on.id)
		return slices.Contains(m.Runtimes, agent.MiseRuntime) || slices.Contains(m.InstallableRuntimes, agent.MiseRuntime)
	})
	m := macByID(e, off.id)
	if slices.Contains(m.Runtimes, agent.MiseRuntime) || slices.Contains(m.InstallableRuntimes, agent.MiseRuntime) {
		t.Fatalf("off-mac offers mise without mise.enabled: %+v %+v", m.Runtimes, m.InstallableRuntimes)
	}
	task := e.runOne(api.TaskSpec{Args: []string{"hello", "managed", "{out:o.txt}"},
		Inputs: e.miseInputs("reserved", helloToml, "lockfile_version = 4\n"), Outputs: []api.Output{{Name: "o.txt"}}})
	logs := e.logs(task.ID)
	if task.State != api.StateSucceeded || task.AgentID != on.id || !strings.Contains(logs, "fake-mise install") ||
		!strings.Contains(logs, "MISE_SAFE=1") {
		t.Fatalf("task %s on %s: %s\n%s", task.State, task.AgentID, e.taskError(task.ID), logs)
	}
	if d, _ := e.app.Task(e.ctx, task.ID); d != nil {
		for _, a := range d.Attempts {
			if a.AgentID == off.id {
				t.Errorf("an attempt ran on off-mac: %+v", a)
			}
		}
	}
}

// miseHostChecksOff turns off the macOS host checks (home folders, disk
// reserve, work volume), which a CI runner does not pass.
func miseHostChecksOff(c *agent.Config) {
	off, zero := false, 0.0
	c.HomeCheck.Disabled, c.WorkVolume.Enabled = true, &off
	c.MinFreeDisk.Min, c.MinFreeDisk.Percent = "0", &zero
}

// A Mac whose network fails the install (here GitHub's per-IP rate limit)
// is at fault, not the task: the attempt is unavailable, no failure is
// counted, nothing is quarantined, and the task runs on another Mac.
func TestMiseNetworkFailureIsMacFault(t *testing.T) {
	skipNoShell(t)
	e := newEnv(t)
	bin := installFakeMise(t, fakeMise)
	limited, _ := e.miseMac("limited-mac", bin, func(c *agent.Config) {
		c.TaskEnv["FAKE_MISE_RECORD"] = t.TempDir()
		c.TaskEnv["FAKE_MISE_FAIL"] = "Failed to install aqua:example/hello@1.0.0: HTTP status client error " +
			"(403 Forbidden) for url (https://api.github.com/repos/example/hello/releases/tags/v1.0.0): API rate limit exceeded"
	})
	zero := 0
	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Name: "mise", Tasks: []api.TaskSpec{{Key: "t", MaxRetries: &zero,
		Runtime: agent.MiseRuntime, Args: []string{"hello", "x", "{out:o.txt}"},
		Inputs: e.miseInputs("net", helloToml, "lockfile_version = 4\n"), Outputs: []api.Output{{Name: "o.txt"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.app.Job(e.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The attempt is "running" until the agent's report reaches the router,
	// which then records the final outcome in one step.
	a, d := e.endedAttempt(j.Tasks[0].ID, 1)
	if a.AgentID != limited.id || a.Outcome != api.OutcomeUnavailable || d.Failures != 0 {
		t.Fatalf("attempt on limited-mac: %+v, failures %d", a, d.Failures)
	}
	good, _ := e.miseMac("good-mac", bin, func(c *agent.Config) { c.TaskEnv["FAKE_MISE_RECORD"] = t.TempDir() })
	final, err := e.app.WaitJob(e.ctx, job.ID, 100*time.Millisecond, nil)
	if err != nil || final.State != api.StateSucceeded || final.Tasks[0].AgentID != good.id {
		t.Fatalf("job: %v %+v", err, final)
	}
}

// installFakeMise installs a mise script the way an admin installs mise:
// neither the file nor its directory writable by the agent's user (the
// agent checks).
func installFakeMise(t *testing.T, script string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "mise")
	if err := os.WriteFile(bin, []byte(script), 0o555); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	return bin
}

// The Mac only offers mise when its binary is one the agent's user cannot
// change and a version the agent was tested with (its JSON output is what
// the agent reads).
func TestMisePreflight(t *testing.T) {
	skipNoShell(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write anything")
	}
	e := newEnv(t)
	writable := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(writable, []byte(fakeMise), 0o755); err != nil {
		t.Fatal(err)
	}
	newer := installFakeMise(t, strings.Replace(fakeMise, "2026.11.9 linux-x64", "2027.1.0 linux-x64", 1))
	for _, c := range []struct{ name, bin, want string }{
		{"writable-mac", writable, "writable by the agent's user"},
		{"newer-mac", newer, "not a version"},
	} {
		f := e.macCfg(c.name, 1, quietTel(), nil, func(cfg *agent.Config) {
			cfg.Mise = agent.MiseConfig{Enabled: true, Binary: c.bin, CacheDir: filepath.Join(t.TempDir(), "cache")}
			miseHostChecksOff(cfg)
		})
		waitFor(t, c.name+" to report mise unavailable", 5*time.Second, func() bool {
			for _, u := range macByID(e, f.id).UnavailableRuntimes {
				if u.Name == agent.MiseRuntime && strings.Contains(u.Reason, c.want) {
					return true
				}
			}
			return false
		})
	}
	// An admin who checked a newer mise lists it.
	ok := e.macCfg("listed-mac", 1, quietTel(), nil, func(cfg *agent.Config) {
		cfg.Mise = agent.MiseConfig{Enabled: true, Binary: newer, CacheDir: filepath.Join(t.TempDir(), "cache"),
			Versions: []string{"2026.11.9", "2027.1.0"}}
		miseHostChecksOff(cfg)
	})
	waitFor(t, "listed-mac to offer mise", 5*time.Second, func() bool {
		return slices.Contains(macByID(e, ok.id).Runtimes, agent.MiseRuntime)
	})
}

// endedAttempt waits until attempt n of a task has its final outcome (not
// running) and returns it with the task.
func (e *env) endedAttempt(taskID string, n int) (api.Attempt, *client.TaskDetail) {
	e.t.Helper()
	var a api.Attempt
	var d *client.TaskDetail
	waitFor(e.t, fmt.Sprintf("attempt %d of %s to end", n, taskID), 30*time.Second, func() bool {
		d, _ = e.app.Task(e.ctx, taskID)
		if d == nil || len(d.Attempts) < n {
			return false
		}
		a = d.Attempts[n-1]
		return a.Outcome != "" && a.Outcome != api.OutcomeRunning
	})
	return a, d
}
