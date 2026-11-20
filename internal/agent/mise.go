package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// mise toolchains: a task with runtime "mise" ships a mise.toml (and, by
// default, a mise.lock with checksums) as inputs. The agent has the
// admin's mise binary do the work: mise reads the config with MISE_SAFE
// (project config may not run code), reports what it contains, installs
// the tools (from any mise backend) locked into the task's own work dir and
// runs the task's program with `mise exec`. The agent only checks mise's
// reports. The functions here are pure; the wiring is in mise_run.go.

// MiseRuntime is the runtime name tasks use for a mise toolchain. It is
// reserved: it only ever runs through the protected path, and only on Macs
// with mise.enabled.
const MiseRuntime = "mise"

const miseOffReason = "reserved for mise toolchains, which are not enabled on this Mac (mise.enabled)"

// The inputs a mise task provides.
const (
	MiseConfigInput = "mise.toml"
	MiseLockInput   = "mise.lock"
)

// maxMiseFile caps mise.toml and mise.lock.
const maxMiseFile = 256 << 10

// MiseConfig is the agent.yaml block that enables mise toolchains. It is
// off by default: a mise task installs tools from any mise backend and
// runs programs they provide, so an admin opts each Mac in.
type MiseConfig struct {
	Enabled bool `yaml:"enabled"`
	// Binary is the mise executable (an absolute path). It becomes the
	// runtime named "mise". Empty: whatever provides the runtime "mise"
	// on this Mac (a `runtimes` entry or a managed runtime command).
	Binary string `yaml:"binary"`
	// AllowTools lists the tools tasks may install, by the backend mise
	// resolves each to (`mise tool NAME`): "core:node", "aqua:jqlang/jq",
	// a prefix with a trailing * ("aqua:*", "github:astral-sh/*"), or "*"
	// for every backend. Default ["core:*", "aqua:*"]: mise's own core
	// tools and aqua packages, which is what most registry names resolve to.
	AllowTools []string `yaml:"allow_tools"`
	// Commands optionally narrows which programs tasks may run (bare
	// names such as "node" or "ffmpeg"). Empty: any program an installed
	// tool provides. Programs outside the tools never run either way.
	Commands []string `yaml:"commands"`
	// RequireLockfile: nil/true = mise installs with --locked, so a task
	// must ship mise.lock with a URL and checksum for this Mac's platform
	// for every tool. false = versions resolve online and no downloads are
	// shared between tasks.
	RequireLockfile *bool `yaml:"require_lockfile"`
	// CacheDir holds mise's download cache, shared by tasks (only with
	// require_lockfile). Default: mise-cache next to work_dir, e.g.
	// /usr/local/var/umm/mise-cache.
	CacheDir string `yaml:"cache_dir"`
	// MaxCache caps the download cache; mise drops the least recently
	// used downloads first. Default 2GiB.
	MaxCache string `yaml:"max_cache"`
	// GitHubTokenFile optionally names a file (mode 0600) holding a GitHub
	// token for tools from private repositories. It is given to
	// `mise install` only (as MISE_GITHUB_TOKEN), never to the task's
	// program or logged. Tasks run as the agent's user, so they could read
	// the file itself: use a fine-grained, read-only token.
	GitHubTokenFile string `yaml:"github_token_file"`
	// GitHubTokenCommand instead runs this argv (e.g. a secrets manager's
	// CLI) before each install; its standard output is the token. Same
	// rules as GitHubTokenFile; set at most one of the two.
	GitHubTokenCommand []string `yaml:"github_token_command"`

	MaxCacheBytes uint64 `yaml:"-"`
}

// Locked reports whether installs run locked.
func (m MiseConfig) Locked() bool { return m.RequireLockfile == nil || *m.RequireLockfile }

var (
	misePatternRe = regexp.MustCompile(`^(\*|[a-z][a-z0-9-]{0,31}:[A-Za-z0-9@._/-]{0,200}\*?)$`)
	miseCommandRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
)

// normalize validates the block and fills defaults. agentDir is the work
// dir's parent.
func (m *MiseConfig) normalize(agentDir string) error {
	if !m.Enabled {
		return nil
	}
	if m.Binary != "" && !strings.HasPrefix(m.Binary, "/") {
		return errors.New("mise.binary must be an absolute path")
	}
	if m.GitHubTokenFile != "" && !strings.HasPrefix(m.GitHubTokenFile, "/") {
		return errors.New("mise.github_token_file must be an absolute path")
	}
	if len(m.GitHubTokenCommand) > 0 {
		if m.GitHubTokenFile != "" {
			return errors.New("set mise.github_token_file or mise.github_token_command, not both")
		}
		if !strings.HasPrefix(m.GitHubTokenCommand[0], "/") {
			return errors.New("mise.github_token_command must start with an absolute executable path")
		}
	}
	if len(m.AllowTools) == 0 {
		m.AllowTools = []string{"core:*", "aqua:*"}
	}
	for _, p := range m.AllowTools {
		if !misePatternRe.MatchString(p) || strings.HasSuffix(p, ":") {
			return fmt.Errorf("mise.allow_tools: %q is not backend:name (e.g. core:node, aqua:*, or *)", p)
		}
	}
	for _, c := range m.Commands {
		if !miseCommandRe.MatchString(c) {
			return fmt.Errorf("mise.commands: %q must be a bare program name", c)
		}
	}
	if m.CacheDir == "" {
		m.CacheDir = path.Join(agentDir, "mise-cache")
	}
	if !strings.HasPrefix(m.CacheDir, "/") && !filepath.IsAbs(m.CacheDir) {
		return errors.New("mise.cache_dir must be absolute")
	}
	if m.MaxCache == "" {
		m.MaxCache = "2GiB"
	}
	n, err := ParseSize(m.MaxCache)
	if err != nil {
		return fmt.Errorf("mise.max_cache: %w", err)
	}
	if n < 1<<20 {
		return errors.New("mise.max_cache must be at least 1MiB")
	}
	m.MaxCacheBytes = n
	return nil
}

// CheckMiseKeys checks `mise config explain --json` of the task's
// mise.toml: only [tools] (and min_version) may be set. [env], [tasks],
// [hooks], [settings], plugins and aliases are refused rather than left to
// MISE_SAFE. Tool options are mise's to police: MISE_SAFE refuses the ones
// that run code or change the install environment (postinstall,
// install_env).
// It returns the tools the file lists.
func CheckMiseKeys(explain []byte) ([]string, error) {
	var keys []struct {
		Path []string `json:"path"`
	}
	if err := json.Unmarshal(explain, &keys); err != nil {
		return nil, fmt.Errorf("unexpected `mise config explain` output: %w", err)
	}
	var tools []string
	for _, k := range keys {
		p := k.Path
		switch {
		case len(p) == 0:
		case p[0] == "min_version" && (len(p) == 1 || (len(p) == 2 && (p[1] == "hard" || p[1] == "soft"))):
		case p[0] == "tools":
			if len(p) == 2 {
				tools = append(tools, p[1])
			}
		default:
			return nil, fmt.Errorf("mise.toml: %s is not allowed (only [tools] and min_version)", strings.Join(p, "."))
		}
	}
	if len(tools) == 0 {
		return nil, errors.New("mise.toml: [tools] is missing or empty")
	}
	sort.Strings(tools)
	return tools, nil
}

// MiseTools reads `mise ls --current --json`: the tools mise will use.
// Each must come from config, the task's mise.toml (mise is told to read
// no other config; this checks it did not), and every tool the file lists
// (want) must be there: mise leaves out tools it cannot resolve, e.g. one
// missing from mise.lock in locked mode.
func MiseTools(ls []byte, config string, want []string) ([]string, error) {
	var m map[string][]struct {
		Source struct {
			Path string `json:"path"`
		} `json:"source"`
	}
	if err := json.Unmarshal(ls, &m); err != nil {
		return nil, fmt.Errorf("unexpected `mise ls` output: %w", err)
	}
	var tools []string
	for name, vs := range m {
		for _, v := range vs {
			if filepath.Clean(v.Source.Path) != filepath.Clean(config) {
				return nil, fmt.Errorf("%s comes from %q, not the task's mise.toml", name, v.Source.Path)
			}
		}
		tools = append(tools, name)
	}
	for _, w := range want {
		if _, ok := m[w]; !ok {
			return nil, fmt.Errorf("mise cannot resolve %s (is it in mise.lock for this platform? mise's warnings are in the task log)", w)
		}
	}
	if len(tools) == 0 {
		return nil, errors.New("mise.toml: [tools] is missing or empty")
	}
	sort.Strings(tools)
	return tools, nil
}

// CheckMiseCommand returns the program a mise task runs (its first
// argument): a bare name, allowed by commands when that is set.
func CheckMiseCommand(args, commands []string) (string, error) {
	if len(args) == 0 || args[0] == "" {
		return "", errors.New(`a mise task's first argument is the program to run (e.g. ["node", "{in:main.js}"])`)
	}
	c := args[0]
	if !miseCommandRe.MatchString(c) {
		return "", fmt.Errorf("program %q must be a bare name provided by one of the task's tools", c)
	}
	if len(commands) > 0 {
		for _, ok := range commands {
			if ok == c {
				return c, nil
			}
		}
		return "", fmt.Errorf("program %s is not allowed on this Mac (allowed: %s)", c, strings.Join(commands, ", "))
	}
	return c, nil
}

// CheckMiseTaskEnv refuses task variables that would change how mise
// behaves (MISE_*: another config, data dir, auto-install, a token, ...)
// and GitHub tokens, which are stored with the job: private tools get
// theirs from the Mac's mise.github_token_file.
func CheckMiseTaskEnv(env map[string]string) error {
	for k := range env {
		if strings.HasPrefix(k, "MISE_") || k == "GITHUB_TOKEN" || k == "GITHUB_API_TOKEN" {
			return fmt.Errorf("a mise task may not set %s", k)
		}
	}
	return nil
}

// misePaths are where one task's mise state lives, inside its work dir,
// so it goes away with it.
type misePaths struct {
	Root  string // <workdir>/.umm-mise
	Data  string // installs
	Cache string // the task's cache; its downloads-cas links to the shared one
}

func newMisePaths(workdir string) misePaths {
	root := filepath.Join(workdir, ".umm-mise")
	return misePaths{Root: root, Data: filepath.Join(root, "data"), Cache: filepath.Join(root, "cache")}
}

// MiseEnv is the environment mise runs with (install and exec). Every
// directory mise would read config or tools from is the task's own, so
// only the task's mise.toml and mise.lock count: no global, system or
// parent-directory config (MISE_CEILING_PATHS is the work dir's parent),
// no tools from the system install dir, no installs while running.
// MISE_SAFE stops project config from running code (env templates, hooks,
// postinstall). No sudo, no calls to mise's version host.
func MiseEnv(p misePaths, ceiling string, locked bool, maxCacheBytes uint64) map[string]string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	cfg := filepath.Join(p.Root, "config")
	return map[string]string{
		"MISE_DATA_DIR":           p.Data,
		"MISE_CACHE_DIR":          p.Cache,
		"MISE_STATE_DIR":          filepath.Join(p.Root, "state"),
		"MISE_CONFIG_DIR":         cfg,
		"MISE_GLOBAL_CONFIG_FILE": filepath.Join(cfg, "none.toml"),
		"MISE_SYSTEM_CONFIG_FILE": filepath.Join(cfg, "none-system.toml"),
		"MISE_SYSTEM_DATA_DIR":    filepath.Join(p.Root, "system"),
		"MISE_CEILING_PATHS":      ceiling,
		// Only mise.toml counts as project config: no .tool-versions,
		// mise.local.toml, .mise.toml, mise.<env>.toml or idiomatic files.
		"MISE_OVERRIDE_CONFIG_FILENAMES":        "mise.toml",
		"MISE_OVERRIDE_TOOL_VERSIONS_FILENAMES": "none",
		"MISE_SAFE":                             "1",
		"MISE_LOCKED":                           b(locked),
		"MISE_AUTO_INSTALL":                     "0",
		"MISE_EXEC_AUTO_INSTALL":                "0",
		"MISE_NOT_FOUND_AUTO_INSTALL":           "0",
		"MISE_SYSTEM_PACKAGES_SUDO":             "0",
		"MISE_SYSTEM_DEPS":                      "warn",
		"MISE_USE_VERSIONS_HOST":                "0",
		"MISE_USE_VERSIONS_HOST_TRACK":          "0",
		"MISE_NETRC":                            "0",
		"MISE_GITHUB_GH_CLI_TOKENS":             "0",
		"MISE_DOWNLOAD_CACHE":                   b(locked),
		"MISE_DOWNLOAD_CACHE_MAX_SIZE":          fmt.Sprintf("%dMiB", maxCacheBytes>>20),
		"MISE_COLOR":                            "0",
		"MISE_TERMINAL_PROGRESS":                "0",
		"NO_COLOR":                              "1",
	}
}

// withEnv returns env (KEY=VALUE entries) with set applied on top.
func withEnv(env []string, set map[string]string) []string {
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, over := set[k]; !over {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return out
}

// insideDir reports whether p is strictly inside dir (lexically).
func insideDir(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsAbs(p) && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// ParseGitHubToken checks what a token file or command produced: one
// non-empty line of printable characters. The error never quotes it.
func ParseGitHubToken(b []byte) (string, error) {
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("the GitHub token is empty")
	}
	if len(tok) > 4096 {
		return "", errors.New("the GitHub token is longer than 4096 bytes")
	}
	for _, r := range tok {
		if r <= ' ' || r == 0x7f {
			return "", errors.New("the GitHub token must be a single word on one line")
		}
	}
	return tok, nil
}

// CheckMiseConfigFiles reads `mise config ls --json`: the config files
// mise loaded. It must be exactly the task's mise.toml (config).
func CheckMiseConfigFiles(ls []byte, config string) error {
	var files []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(ls, &files); err != nil {
		return fmt.Errorf("unexpected `mise config ls` output: %w", err)
	}
	var other []string
	found := false
	for _, f := range files {
		if filepath.Clean(f.Path) == filepath.Clean(config) {
			found = true
		} else {
			other = append(other, f.Path)
		}
	}
	if len(other) > 0 {
		return fmt.Errorf("mise loaded config besides the task's mise.toml: %s", strings.Join(other, ", "))
	}
	if !found {
		return errors.New("mise did not load the task's mise.toml")
	}
	return nil
}

// MiseToolAllowed reports whether allow permits a tool's backend.
func MiseToolAllowed(allow []string, backend string) bool {
	for _, p := range allow {
		if p == backend || (strings.HasSuffix(p, "*") && strings.HasPrefix(backend, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}

// MiseBackend reads `mise tool NAME --json`: the backend mise installs the
// tool with.
func MiseBackend(tool []byte) (string, error) {
	var t struct {
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(tool, &t); err != nil || t.Backend == "" {
		return "", errors.New("unexpected `mise tool` output")
	}
	return t.Backend, nil
}

// miseProgramInside resolves every link in path and checks the program it
// leads to is inside installs (resolved too): a tool's bin/node may be a
// link to /bin/sh. It returns the resolved path.
func miseProgramInside(path, installs string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(installs)
	if err != nil {
		return "", err
	}
	if !insideDir(real, root) {
		return "", fmt.Errorf("%s leads to %s, outside the installed tools", path, real)
	}
	return real, nil
}
