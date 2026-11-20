package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are trimmed output of mise 2026.11.9 (`config explain
// --json`, `ls --current --json`).

func TestCheckMiseKeys(t *testing.T) {
	cases := []struct {
		name, json, err string
	}{
		{"tools and min_version", `[{"key":"min_version","path":["min_version"]},{"key":"min_version.hard","path":["min_version","hard"]},
			{"key":"tools","path":["tools"]},{"key":"tools.node","path":["tools","node"]}]`, ""},
		{"tool options are mise's to police", `[{"key":"tools","path":["tools"]},{"key":"tools.aqua:jqlang/jq","path":["tools","aqua:jqlang/jq"]},
			{"key":"tools.aqua:jqlang/jq.version","path":["tools","aqua:jqlang/jq","version"]},
			{"key":"tools.aqua:jqlang/jq.exe","path":["tools","aqua:jqlang/jq","exe"]}]`, ""},
		{"env", `[{"key":"env","path":["env"]},{"key":"env.A","path":["env","A"]},{"key":"tools","path":["tools"]},
			{"key":"tools.node","path":["tools","node"]}]`, "env is not allowed"},
		{"tasks", `[{"key":"tasks","path":["tasks"]},{"key":"tasks.b","path":["tasks","b"]},{"key":"tasks.b.run","path":["tasks","b","run"]},
			{"key":"tools","path":["tools"]},{"key":"tools.node","path":["tools","node"]}]`, "tasks is not allowed"},
		{"hooks", `[{"key":"hooks","path":["hooks"]},{"key":"hooks.enter","path":["hooks","enter"]},{"key":"tools","path":["tools"]},
			{"key":"tools.node","path":["tools","node"]}]`, "hooks is not allowed"},
		{"settings", `[{"key":"settings","path":["settings"]},{"key":"settings.locked","path":["settings","locked"]},
			{"key":"tools","path":["tools"]},{"key":"tools.node","path":["tools","node"]}]`, "settings is not allowed"},
		{"min_version other key", `[{"key":"min_version.x","path":["min_version","x"]},{"key":"tools.node","path":["tools","node"]}]`,
			"min_version.x is not allowed"},
		{"no tools", `[{"key":"min_version","path":["min_version"]}]`, "[tools] is missing"},
		{"empty tools table", `[{"key":"tools","path":["tools"]}]`, "[tools] is missing"},
		{"not json", `mise ERROR boom`, "unexpected"},
	}
	for _, c := range cases {
		_, err := CheckMiseKeys([]byte(c.json))
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.err)
		}
	}
	tools, _ := CheckMiseKeys([]byte(`[{"key":"tools","path":["tools"]},{"key":"tools.node","path":["tools","node"]},
		{"key":"tools.aqua:jqlang/jq","path":["tools","aqua:jqlang/jq"]},{"key":"tools.aqua:jqlang/jq.exe","path":["tools","aqua:jqlang/jq","exe"]}]`))
	if strings.Join(tools, ",") != "aqua:jqlang/jq,node" {
		t.Errorf("tools %v", tools)
	}
}

func TestMiseTools(t *testing.T) {
	cfg := "/w/t1-1/mise.toml"
	ls := func(tool, version, src string) string {
		return `{"` + tool + `":[{"version":"x","requested_version":"` + version + `","install_path":"/w/t1-1/.umm-mise/data/installs/x",` +
			`"source":{"type":"mise.toml","path":"` + src + `"},"installed":false,"active":false}]}`
	}
	got, err := MiseTools([]byte(`{"node":[{"requested_version":"22.11.0","source":{"type":"mise.toml","path":"/w/t1-1/mise.toml"}}],
		"aqua:jqlang/jq":[{"requested_version":"1.7.1","source":{"type":"mise.toml","path":"/w/t1-1/mise.toml"}}]}`), cfg,
		[]string{"node", "aqua:jqlang/jq"})
	if err != nil || strings.Join(got, ",") != "aqua:jqlang/jq,node" {
		t.Errorf("tools %v, %v", got, err)
	}
	for _, c := range []struct {
		name, json, err string
		want            []string
	}{
		{"other config", ls("node", "22", "/etc/mise/config.toml"), "not the task's mise.toml", nil},
		{"no source", ls("node", "22", ""), "not the task's mise.toml", nil},
		{"not resolved", ls("node", "22", cfg), "cannot resolve aqua:jqlang/jq", []string{"node", "aqua:jqlang/jq"}},
		{"none", `{}`, "[tools] is missing", nil},
		{"none resolved", `{}`, "cannot resolve node", []string{"node"}},
		{"not json", ``, "unexpected", nil},
	} {
		_, err := MiseTools([]byte(c.json), cfg, c.want)
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.err)
		}
	}
}

func TestCheckMiseCommand(t *testing.T) {
	cases := []struct {
		args, commands []string
		want, err      string
	}{
		{[]string{"node", "main.js"}, nil, "node", ""},
		{[]string{"ffmpeg", "-i", "x"}, []string{"ffmpeg", "ffprobe"}, "ffmpeg", ""},
		{[]string{"node"}, []string{"ffmpeg"}, "", "not allowed on this Mac"},
		{[]string{"/bin/sh", "-c", "id"}, nil, "", "bare name"},
		{[]string{"../node"}, nil, "", "bare name"},
		{[]string{"-e"}, nil, "", "bare name"},
		{nil, nil, "", "first argument"},
		{[]string{""}, nil, "", "first argument"},
	}
	for _, c := range cases {
		got, err := CheckMiseCommand(c.args, c.commands)
		if c.err == "" && (err != nil || got != c.want) {
			t.Errorf("%v: %q, %v; want %q", c.args, got, err, c.want)
		}
		if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%v: err %v, want %q", c.args, err, c.err)
		}
	}
}

func TestCheckMiseTaskEnv(t *testing.T) {
	if err := CheckMiseTaskEnv(map[string]string{"FOO": "1", "MISE": "x"}); err != nil {
		t.Error(err)
	}
	for _, k := range []string{"MISE_ENV", "MISE_DATA_DIR", "MISE_EXEC_AUTO_INSTALL", "MISE_SAFE", "MISE_GITHUB_TOKEN",
		"GITHUB_TOKEN", "GITHUB_API_TOKEN"} {
		if err := CheckMiseTaskEnv(map[string]string{k: "1"}); err == nil {
			t.Errorf("%s accepted", k)
		}
	}
}

func TestMiseEnv(t *testing.T) {
	wd := filepath.Join("/", "w", "t1-1")
	env := MiseEnv(newMisePaths(wd), "/w", true, 2<<30)
	for k, want := range map[string]string{
		"MISE_DATA_DIR": filepath.Join(wd, ".umm-mise", "data"), "MISE_CACHE_DIR": filepath.Join(wd, ".umm-mise", "cache"),
		"MISE_CEILING_PATHS": "/w", "MISE_SAFE": "1", "MISE_LOCKED": "1", "MISE_DOWNLOAD_CACHE": "1",
		"MISE_EXEC_AUTO_INSTALL": "0", "MISE_AUTO_INSTALL": "0", "MISE_NOT_FOUND_AUTO_INSTALL": "0",
		"MISE_SYSTEM_PACKAGES_SUDO": "0", "MISE_USE_VERSIONS_HOST": "0", "MISE_DOWNLOAD_CACHE_MAX_SIZE": "2048MiB",
		// Only a file named mise.toml is project config: no .tool-versions,
		// mise.local.toml, .mise.toml or mise.<env>.toml.
		"MISE_OVERRIDE_CONFIG_FILENAMES": "mise.toml", "MISE_OVERRIDE_TOOL_VERSIONS_FILENAMES": "none",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	// Every place mise reads config or tools from is the task's own.
	for _, k := range []string{"MISE_DATA_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR", "MISE_CONFIG_DIR",
		"MISE_GLOBAL_CONFIG_FILE", "MISE_SYSTEM_CONFIG_FILE", "MISE_SYSTEM_DATA_DIR"} {
		if !insideDir(env[k], wd) {
			t.Errorf("%s = %s is not inside the task's work dir", k, env[k])
		}
	}
	env = MiseEnv(newMisePaths(wd), "/w", false, 2<<30)
	if env["MISE_LOCKED"] != "0" || env["MISE_DOWNLOAD_CACHE"] != "0" {
		t.Errorf("unlocked: %v", env)
	}
}

func TestWithEnv(t *testing.T) {
	got := withEnv([]string{"A=1", "MISE_DATA_DIR=/evil", "B=2=3"}, map[string]string{"MISE_DATA_DIR": "/ok", "C": "4"})
	if strings.Join(got, " ") != "A=1 B=2=3 C=4 MISE_DATA_DIR=/ok" {
		t.Errorf("withEnv: %v", got)
	}
}

func TestInsideDir(t *testing.T) {
	for _, c := range []struct {
		p, dir string
		want   bool
	}{
		{"/w/t/.umm-mise/data/installs/node/bin/node", "/w/t/.umm-mise/data/installs", true},
		{"/w/t/.umm-mise/data/installs", "/w/t/.umm-mise/data/installs", false},
		{"/w/t/.umm-mise/data/installs2/x", "/w/t/.umm-mise/data/installs", false},
		{"/w/t/.umm-mise/data/installs/../../x", "/w/t/.umm-mise/data/installs", false},
		{"/usr/bin/node", "/w/t/.umm-mise/data/installs", false},
		{"node", "/w/t/.umm-mise/data/installs", false},
		{"", "/w/t/.umm-mise/data/installs", false},
	} {
		if got := insideDir(c.p, c.dir); got != c.want {
			t.Errorf("insideDir(%q, %s) = %v", c.p, c.dir, got)
		}
	}
}

func TestMiseConfigNormalize(t *testing.T) {
	base := func() *Config {
		return &Config{Server: "https://umm.example.com", WorkDir: "/usr/local/var/umm/work",
			Runtimes: map[string][]string{"ffmpeg": {"/opt/homebrew/bin/ffmpeg"}}}
	}
	// Off by default: no runtime, nothing required.
	c := base()
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Runtimes[MiseRuntime]; ok || c.Mise.Enabled {
		t.Error("mise must be off by default")
	}

	c = base()
	c.Mise = MiseConfig{Enabled: true, Binary: "/opt/homebrew/bin/mise"}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := c.Runtimes[MiseRuntime]; len(got) != 1 || got[0] != "/opt/homebrew/bin/mise" {
		t.Errorf("runtime mise = %v", got)
	}
	if c.Mise.CacheDir != "/usr/local/var/umm/mise-cache" || c.Mise.MaxCacheBytes != 2<<30 || !c.Mise.Locked() {
		t.Errorf("defaults: %+v", c.Mise)
	}
	if err := c.Normalize(); err != nil { // idempotent
		t.Errorf("second Normalize: %v", err)
	}

	// Without binary, mise comes from a managed runtime.
	c = base()
	c.Mise = MiseConfig{Enabled: true}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Runtimes[MiseRuntime]; ok {
		t.Error("no binary: nothing to add")
	}

	bad := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"relative binary", func(c *Config) {
			c.Mise = MiseConfig{Enabled: true, Binary: "mise"}
		}, "absolute"},
		{"relative token file", func(c *Config) {
			c.Mise = MiseConfig{Enabled: true, GitHubTokenFile: "token"}
		}, "github_token_file must be an absolute path"},
		{"relative token command", func(c *Config) {
			c.Mise = MiseConfig{Enabled: true, GitHubTokenCommand: []string{"vault", "kv", "get"}}
		}, "absolute executable path"},
		{"token file and command", func(c *Config) {
			c.Mise = MiseConfig{Enabled: true, GitHubTokenFile: "/etc/t", GitHubTokenCommand: []string{"/usr/bin/true"}}
		}, "not both"},
		{"bad command", func(c *Config) {
			c.Mise = MiseConfig{Enabled: true, Commands: []string{"/bin/sh"}}
		}, "bare program name"},
		{"cache in work dir", func(c *Config) {
			c.Mise = MiseConfig{Enabled: true, CacheDir: "/usr/local/var/umm/work/cache"}
		}, "inside work_dir"},
		{"relative cache", func(c *Config) {
			c.Mise = MiseConfig{Enabled: true, CacheDir: "cache"}
		}, "absolute"},
		{"bad max_cache", func(c *Config) {
			c.Mise = MiseConfig{Enabled: true, MaxCache: "lots"}
		}, "max_cache"},
		{"tiny max_cache", func(c *Config) {
			c.Mise = MiseConfig{Enabled: true, MaxCache: "1000"}
		}, "at least 1MiB"},
		{"runtime named mise, mise off", func(c *Config) {
			c.Runtimes[MiseRuntime] = []string{"/usr/local/bin/mise"}
		}, "reserved"},
		{"runtime named mise, mise on", func(c *Config) {
			c.Runtimes[MiseRuntime] = []string{"/usr/local/bin/mise"}
			c.Mise = MiseConfig{Enabled: true}
		}, "reserved"},
		{"two mise runtimes", func(c *Config) {
			c.Runtimes[MiseRuntime] = []string{"/usr/local/bin/mise"}
			c.Mise = MiseConfig{Enabled: true, Binary: "/opt/homebrew/bin/mise"}
		}, "reserved"},
	}
	for _, b := range bad {
		c := base()
		b.mut(c)
		err := c.Normalize()
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s: err %v, want %q", b.name, err, b.want)
		}
	}
}

func TestParseGitHubToken(t *testing.T) {
	if tok, err := ParseGitHubToken([]byte("  github_pat_abc123\n")); err != nil || tok != "github_pat_abc123" {
		t.Errorf("token %q, %v", tok, err)
	}
	for _, bad := range []string{"", " \n", "two words", "line1\nline2", "tab\there", strings.Repeat("x", 5000)} {
		_, err := ParseGitHubToken([]byte(bad))
		if err == nil {
			t.Errorf("%q accepted", bad)
		} else if len(bad) > 3 && strings.Contains(err.Error(), strings.TrimSpace(bad)) {
			t.Errorf("error quotes the token: %v", err)
		}
	}
}

func TestCheckMiseConfigFiles(t *testing.T) {
	cfg := "/w/t1-1/mise.toml"
	ok := `[{"path":"/w/t1-1/mise.toml","tools":["aqua:jqlang/jq"],"override":false}]`
	if err := CheckMiseConfigFiles([]byte(ok), cfg); err != nil {
		t.Error(err)
	}
	for _, c := range []struct{ name, json string }{
		{"local file too", `[{"path":"/w/t1-1/mise.local.toml","tools":[]},{"path":"/w/t1-1/mise.toml","tools":["node"]}]`},
		{"tool-versions", `[{"path":"/w/t1-1/mise.toml","tools":["node"]},{"path":"/w/t1-1/.tool-versions","tools":["node"]}]`},
		{"work volume root", `[{"path":"/w/mise.toml","tools":[]},{"path":"/w/t1-1/mise.toml","tools":["node"]}]`},
		{"global", `[{"path":"/etc/mise/config.toml","tools":["node"]}]`},
		{"none", `[]`},
		{"not json", `nope`},
	} {
		if err := CheckMiseConfigFiles([]byte(c.json), cfg); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}
