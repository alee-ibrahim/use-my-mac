// Package installer renders the one-line Mac installer served at
// /i/<token>: a POSIX sh script with the router URL, the install token, the
// agent version, the release public key and the install profile embedded.
//
// Every value that ends up in the script is validated against a strict
// pattern first, then single-quoted (shell) or yaml-encoded inside a quoted
// heredoc with a random delimiter, so nothing from a request or a profile can
// break out of its slot.
package installer

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// DefaultServiceLabel is the LaunchDaemon label when a profile sets none
// (the same default as deploy/mac/install.sh and `umm fleet`).
const DefaultServiceLabel = "io.github.alee-ibrahim.umm-agent"

// Bundle is the release bundle the installer downloads.
const Bundle = "umm-agent-darwin-arm64"

//go:embed verify.js
var verifyJS string

var (
	labelKeyRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)
	labelValRe     = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{0,128}$`)
	runtimeNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	argRe          = regexp.MustCompile(`^[A-Za-z0-9._/@+=:,%-]{1,512}$`)
	serviceLabelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{1,200}$`)
	profileNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	tokenRe        = regexp.MustCompile(`^umm_ins_[A-Za-z0-9_-]{20,100}$`)
	versionRe      = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	pubKeyRe       = regexp.MustCompile(`^[A-Za-z0-9+/]{56}$`)
	urlRe          = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)
	rootRe         = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)*$`)
)

// ValidToken reports whether s looks like an install token.
func ValidToken(s string) bool { return tokenRe.MatchString(s) }

// ValidLabels checks label keys and values.
func ValidLabels(l map[string]string) error {
	for k, v := range l {
		if !labelKeyRe.MatchString(k) {
			return fmt.Errorf("label key %q is invalid", k)
		}
		if !labelValRe.MatchString(v) {
			return fmt.Errorf("label %q: value %q is invalid (letters, digits and ._:/@+- only)", k, v)
		}
	}
	return nil
}

// NormalizeProfile fills defaults and validates a profile.
func NormalizeProfile(p *api.InstallProfile) error {
	if !profileNameRe.MatchString(p.Name) {
		return fmt.Errorf("profile name %q is invalid", p.Name)
	}
	if p.MaxTasks <= 0 {
		p.MaxTasks = 1
	}
	if p.MaxTasks > 64 {
		return fmt.Errorf("profile %s: max_tasks must be at most 64", p.Name)
	}
	if p.Priority == "" {
		p.Priority = "adaptive"
	}
	switch p.Priority {
	case "adaptive", "background", "utility", "none":
	default:
		return fmt.Errorf("profile %s: priority must be adaptive, background, utility or none", p.Name)
	}
	if p.ServiceLabel == "" {
		p.ServiceLabel = DefaultServiceLabel
	}
	if !serviceLabelRe.MatchString(p.ServiceLabel) || strings.HasSuffix(p.ServiceLabel, ".updater") {
		return fmt.Errorf("profile %s: service_label %q is invalid", p.Name, p.ServiceLabel)
	}
	if err := ValidLabels(p.Labels); err != nil {
		return fmt.Errorf("profile %s: %w", p.Name, err)
	}
	if len(p.Runtimes) == 0 {
		return fmt.Errorf("profile %s: at least one runtime is required", p.Name)
	}
	for name, argv := range p.Runtimes {
		if !runtimeNameRe.MatchString(name) {
			return fmt.Errorf("profile %s: runtime name %q is invalid", p.Name, name)
		}
		if len(argv) == 0 || !strings.HasPrefix(argv[0], "/") {
			return fmt.Errorf("profile %s: runtime %q must start with an absolute executable path", p.Name, name)
		}
		for _, a := range argv {
			if !argRe.MatchString(a) || strings.Contains(a, "..") {
				return fmt.Errorf("profile %s: runtime %q: argument %q has characters that are not allowed", p.Name, name, a)
			}
		}
	}
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	return nil
}

// Command is the one-liner an admin runs on the Mac.
func Command(router, token string) string {
	return "curl -fsSL " + router + "/i/" + token + " | sudo sh"
}

// UninstallCommand removes the agent (add `-s -- --purge` to sh to purge).
func UninstallCommand(router string) string {
	return "curl -fsSL " + router + "/i/uninstall | sudo sh"
}

// Params is what goes into an install script.
type Params struct {
	Router    string // router public URL (no trailing slash)
	Token     string // install token (the enrolment secret)
	Version   string // agent release to install
	PublicKey string // minisign public key line (base64)
	// ArtifactBase is where SHA256SUMS, SHA256SUMS.minisig and the bundle
	// are: <router>/i/<token>/releases/<version> (relay) or the release host.
	ArtifactBase string
	Profile      api.InstallProfile
	// AllowHTTP permits plain-http downloads (dev mode only).
	AllowHTTP bool
	// Root prefixes every path the script touches. Empty in production;
	// tests point it at a temp dir.
	Root string
}

func sq(s string) string { return "'" + s + "'" }

// agentYAML renders agent.yaml without the name (the script adds the Mac's
// own name). Mirrors deploy/mac/install.sh and `umm fleet`.
func agentYAML(p Params) ([]byte, error) {
	type yieldCfg struct {
		UserCPUHigh     int    `yaml:"user_cpu_high"`
		UserCPULow      int    `yaml:"user_cpu_low"`
		SustainSeconds  int    `yaml:"sustain_seconds"`
		CooldownSeconds int    `yaml:"cooldown_seconds"`
		MemoryPressure  string `yaml:"memory_pressure"`
		Thermal         string `yaml:"thermal"`
		YieldOnBattery  bool   `yaml:"yield_on_battery"`
	}
	cfg := struct {
		Server         string              `yaml:"server"`
		CredentialFile string              `yaml:"credential_file"`
		WorkDir        string              `yaml:"work_dir"`
		MaxTasks       int                 `yaml:"max_tasks"`
		Priority       string              `yaml:"priority"`
		PauseFile      string              `yaml:"pause_file"`
		UserLoadScope  string              `yaml:"user_load_scope"`
		Labels         map[string]string   `yaml:"labels,omitempty"`
		Runtimes       map[string][]string `yaml:"runtimes"`
		Yield          yieldCfg            `yaml:"yield"`
	}{
		Server:         p.Router,
		CredentialFile: p.Root + "/usr/local/etc/umm/credential",
		WorkDir:        p.Root + "/usr/local/var/umm/work",
		MaxTasks:       p.Profile.MaxTasks,
		Priority:       p.Profile.Priority,
		PauseFile:      p.Root + "/usr/local/etc/umm/paused",
		UserLoadScope:  "console",
		Labels:         p.Profile.Labels,
		Runtimes:       p.Profile.Runtimes,
		Yield: yieldCfg{UserCPUHigh: 50, UserCPULow: 25, SustainSeconds: 10, CooldownSeconds: 120,
			MemoryPressure: "warn", Thermal: "serious", YieldOnBattery: true},
	}
	var buf bytes.Buffer
	buf.WriteString("# Written by the use-my-mac one-line installer (profile " + p.Profile.Name + "). No secrets here.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	enc.Close()
	return buf.Bytes(), nil
}

func (p *Params) validate() error {
	switch {
	case !urlRe.MatchString(p.Router):
		return errors.New("installer: router URL is invalid")
	case !strings.HasPrefix(p.Router, "https://") && !p.AllowHTTP:
		return errors.New("installer: router URL must be https")
	case !tokenRe.MatchString(p.Token):
		return errors.New("installer: token is invalid")
	case !versionRe.MatchString(p.Version):
		return errors.New("installer: version is invalid")
	case !pubKeyRe.MatchString(p.PublicKey):
		return errors.New("installer: public key is invalid")
	case !urlRe.MatchString(p.ArtifactBase):
		return errors.New("installer: artifact URL is invalid")
	case !strings.HasPrefix(p.ArtifactBase, "https://") && !p.AllowHTTP:
		return errors.New("installer: artifact URL must be https")
	case !rootRe.MatchString(p.Root):
		return errors.New("installer: root is invalid")
	}
	return NormalizeProfile(&p.Profile)
}

// heredoc returns a delimiter that does not occur in any of the bodies.
func heredoc(bodies ...string) string {
	for {
		b := make([]byte, 8)
		rand.Read(b)
		d := "UMM_EOF_" + hex.EncodeToString(b)
		if !slices.ContainsFunc(bodies, func(s string) bool { return strings.Contains(s, d) }) {
			return d
		}
	}
}

// Render builds the install script.
func Render(p Params) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	cfg, err := agentYAML(p)
	if err != nil {
		return nil, err
	}
	plist := renderPlist(agentPlistTmpl, plistData{p.Profile.ServiceLabel, p.Root})
	upd := renderPlist(updaterPlistTmpl, plistData{p.Profile.ServiceLabel, p.Root})
	proto := "=https"
	if p.AllowHTTP {
		proto = "=https,http"
	}
	runtimes := slices.Sorted(maps.Keys(p.Profile.Runtimes))
	data := map[string]any{
		"Router": sq(p.Router), "Token": sq(p.Token), "Version": sq(p.Version), "Label": sq(p.Profile.ServiceLabel),
		"PubKey": sq(p.PublicKey), "Src": sq(p.ArtifactBase), "Root": sq(p.Root), "Proto": sq(proto),
		"Bundle": sq(Bundle), "Profile": p.Profile.Name, "Runtimes": strings.Join(runtimes, ", "),
		"RouterPlain": p.Router,
		"VerifyJS":    verifyJS, "AgentYAML": string(cfg), "Plist": plist, "UpdaterPlist": upd,
		"EOF": heredoc(verifyJS, string(cfg), plist, upd),
	}
	var buf bytes.Buffer
	if err := installTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var errMsgRe = regexp.MustCompile(`[^A-Za-z0-9 .,:;()/<>_+=-]`)

// RenderError is a script that prints msg and exits non-zero. Used when the
// token is unknown, expired, revoked or used up, so `curl | sudo sh` fails
// loudly instead of running an empty script.
func RenderError(msg string) []byte {
	msg = errMsgRe.ReplaceAllString(msg, "")
	return []byte("#!/bin/sh\n# use-my-mac one-line installer\nprintf '%s\\n' 'use-my-mac: error: " + msg + "' >&2\nexit 1\n")
}

// RenderUninstall builds the uninstall script (root "" in production).
func RenderUninstall(root string) ([]byte, error) {
	if !rootRe.MatchString(root) {
		return nil, errors.New("installer: root is invalid")
	}
	return []byte(strings.Replace(uninstallScript, "R=''", "R="+sq(root), 1)), nil
}

func renderPlist(t *template.Template, d plistData) string {
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		panic(err)
	}
	return buf.String()
}
