package integration

import (
	"bytes"
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
)

// fakeMise stands in for mise where it is not installed (CI). It answers
// the agent's queries for a config with one tool, "hello": `config
// explain` (reporting [env] when the file has one), `ls`, `install` (puts
// a hello program into $MISE_DATA_DIR/installs; fails for a config that
// mentions "broken"; records the GitHub token it got), `which` and `exec`.
// The hello program says whether it can see a token.
const fakeMise = `#!/bin/sh
case "$1" in
config)
	if grep -q '^\[env\]' "$4"; then
		echo '[{"key":"env","path":["env"]},{"key":"env.X","path":["env","X"]},{"key":"tools.hello","path":["tools","hello"]}]'
	else
		echo '[{"key":"tools","path":["tools"]},{"key":"tools.hello","path":["tools","hello"]}]'
	fi
	;;
ls)
	echo '{"hello":[{"requested_version":"1.0.0","source":{"type":"mise.toml","path":"'"$PWD"'/mise.toml"}}]}'
	;;
install)
	echo "fake-mise install"
	env | grep -E '^MISE_(SAFE|LOCKED|EXEC_AUTO_INSTALL|SYSTEM_PACKAGES_SUDO)=' | sort
	[ -n "$MISE_GITHUB_TOKEN" ] && echo "github token: set"
	printf %s "$MISE_GITHUB_TOKEN" > "$FAKE_MISE_RECORD/install-token"
	if grep -q broken mise.toml; then echo "download failed: 404" >&2; exit 1; fi
	bin="$MISE_DATA_DIR/installs/hello/1.0.0/bin"
	mkdir -p "$bin"
	printf '#!/bin/sh\n[ "$1" = exit127 ] && exit 127\necho "hello from the toolchain; task sees token: ${MISE_GITHUB_TOKEN:-none}${GITHUB_TOKEN:-}"\nprintf %%s "$1" > "$2"\n' > "$bin/hello"
	chmod 755 "$bin/hello"
	;;
which)
	p="$MISE_DATA_DIR/installs/hello/1.0.0/bin/$2"
	if [ -x "$p" ]; then echo "$p"; else echo "mise ERROR $2 is not a mise bin" >&2; exit 1; fi
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
	// Several task errors in a row on one Mac must not quarantine it here.
	e := newEnvCfg(t, func(c *engine.Config) { c.QuarantineAfter = 0 })
	dir := t.TempDir()
	bin := filepath.Join(dir, "mise")
	if err := os.WriteFile(bin, []byte(fakeMise), 0o755); err != nil {
		t.Fatal(err)
	}
	const token = "github_pat_test0000000000secret"
	tokenFile := filepath.Join(dir, "github-token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := t.TempDir()
	mac, cache := e.miseMac("mise-mac", bin, func(c *agent.Config) {
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

	// A token from a command (a secrets manager's CLI): only its standard
	// output counts, and it reaches the install only.
	record2 := t.TempDir()
	e.miseMac("cmd-mac", bin, func(c *agent.Config) {
		c.Mise.GitHubTokenCommand = []string{"/bin/sh", "-c", "echo noise-" + token + " >&2; echo " + token + "-cmd"}
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
// (downloaded from GitHub; needs network access to github.com) and
// `mise exec`. A second task gets the download from the shared cache,
// checked against the lockfile again; a lockfile with the wrong checksum
// fails; a program from outside the tools never runs.
func TestMiseRealInstall(t *testing.T) {
	if os.Getenv("UMM_TEST_MISE") == "" {
		t.Skip("set UMM_TEST_MISE=1 to run a real mise install (downloads jq from github.com)")
	}
	bin := realMise(t)
	plat := map[string]string{"darwin/arm64": "macos-arm64", "linux/amd64": "linux-x64", "linux/arm64": "linux-arm64"}[runtime.GOOS+"/"+runtime.GOARCH]
	if plat == "" {
		t.Skipf("no jq checksum in the test lockfile for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	e := newEnvCfg(t, func(c *engine.Config) { c.QuarantineAfter = 0 })
	_, cache := e.miseMac("real-mise", bin, func(c *agent.Config) { c.Mise.Commands = []string{"jq", "sh"} })
	jq := "[tools]\n\"aqua:jqlang/jq\" = \"1.7.1\"\n"
	in := e.miseInputs("jq", jq, realJQLock)
	ts := api.TaskSpec{Args: []string{"jq", "-n", "-c", `{"tool": "jq", "args": $ARGS.positional}`, "--args", "pinned"},
		Inputs: in, TimeoutSeconds: 300}
	for i := 0; i < 2; i++ {
		task := e.runOne(ts)
		logs := e.logs(task.ID)
		if task.State != api.StateSucceeded || !strings.Contains(logs, `{"tool":"jq","args":["pinned"]}`) {
			t.Fatalf("run %d: %s %s\n%s", i, task.State, e.taskError(task.ID), logs)
		}
		t.Logf("run %d log:\n%s", i, logs)
	}
	if ents, err := os.ReadDir(filepath.Join(cache, "downloads-cas", "blobs")); err != nil || len(ents) == 0 {
		t.Errorf("shared download cache is empty: %v", err)
	}

	task := e.runOne(api.TaskSpec{Args: []string{"sh", "-c", "id"}, Inputs: in, TimeoutSeconds: 300})
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
	task = e.runOne(ts)
	if logs := e.logs(task.ID); task.State != api.StateFailed || !strings.Contains(logs, "Checksum mismatch") {
		t.Fatalf("bad checksum: %s %s\n%s", task.State, e.taskError(task.ID), logs)
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
