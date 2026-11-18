package fleet

import (
	"bytes"
	"fmt"
	"text/template"

	"gopkg.in/yaml.v3"
)

// agentConfigFile is where the agent config lives on every Mac.
const agentConfigFile = "/usr/local/etc/umm/agent.yaml"

// RenderAgentConfig builds the agent.yaml staged for a host. It mirrors the
// config written by deploy/mac/install.sh but is populated from the inventory.
// It holds no secrets (the credential is written separately by `umm-agent
// enrol`).
func RenderAgentConfig(router string, h ResolvedHost) ([]byte, error) {
	if router == "" {
		return nil, fmt.Errorf("router URL is required")
	}
	if len(h.Runtimes) == 0 {
		return nil, fmt.Errorf("host %q: at least one runtime must be set (in the inventory defaults or on the host) so the agent config is valid", h.Host)
	}
	// Ordered map via yaml.Node would be ideal; a plain struct keeps it simple
	// and the agent tolerates field order.
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
		Name           string              `yaml:"name"`
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
		Server:         router,
		Name:           h.Name,
		CredentialFile: "/usr/local/etc/umm/credential",
		WorkDir:        "/usr/local/var/umm/work",
		MaxTasks:       h.MaxTasks,
		Priority:       h.Priority,
		PauseFile:      "/usr/local/etc/umm/paused",
		UserLoadScope:  "console",
		Labels:         h.Labels,
		Runtimes:       h.Runtimes,
		Yield: yieldCfg{
			UserCPUHigh: 50, UserCPULow: 25, SustainSeconds: 10, CooldownSeconds: 120,
			MemoryPressure: "warn", Thermal: "serious", YieldOnBattery: true,
		},
	}
	var buf bytes.Buffer
	buf.WriteString("# Rendered by `umm fleet` from the inventory. No secrets here.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	enc.Close()
	return buf.Bytes(), nil
}

var plistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Rendered by umm fleet. Runs the agent as the non-admin _umm user. -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/umm-agent</string>
    <string>run</string>
    <string>-config</string>
    <string>{{.Config}}</string>
  </array>
  <key>UserName</key>
  <string>_umm</string>
  <key>GroupName</key>
  <string>_umm</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>ExitTimeOut</key>
  <integer>30</integer>
  <key>Nice</key>
  <integer>5</integer>
  <key>WorkingDirectory</key>
  <string>/usr/local/var/umm</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>StandardOutPath</key>
  <string>/usr/local/var/log/umm/agent.log</string>
  <key>StandardErrorPath</key>
  <string>/usr/local/var/log/umm/agent.log</string>
</dict>
</plist>
`))

// RenderPlist builds the LaunchDaemon plist with the configured (possibly
// neutral) label.
func RenderPlist(label string) ([]byte, error) {
	if !serviceLabelRe.MatchString(label) {
		return nil, fmt.Errorf("invalid service label %q", label)
	}
	var buf bytes.Buffer
	if err := plistTmpl.Execute(&buf, struct{ Label, Config string }{label, agentConfigFile}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var updaterPlistTmpl = template.Must(template.New("updater").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Rendered by umm fleet. The agent updater: runs as root at boot and every
     15 minutes; verifies signed releases, swaps the binary, rolls back. -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{.Label}}.updater</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/umm-agent</string>
    <string>update</string>
    <string>-config</string>
    <string>{{.Config}}</string>
    <string>-label</string>
    <string>{{.Label}}</string>
    <string>-jitter</string>
    <string>2m</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>StartInterval</key>
  <integer>900</integer>
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
  <string>/usr/local/var/log/umm-updater.log</string>
  <key>StandardErrorPath</key>
  <string>/usr/local/var/log/umm-updater.log</string>
</dict>
</plist>
`))

// RenderUpdaterPlist builds the root updater's LaunchDaemon plist. Its label
// is the agent's label plus ".updater".
func RenderUpdaterPlist(label string) ([]byte, error) {
	if !serviceLabelRe.MatchString(label) {
		return nil, fmt.Errorf("invalid service label %q", label)
	}
	var buf bytes.Buffer
	if err := updaterPlistTmpl.Execute(&buf, struct{ Label, Config string }{label, agentConfigFile}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
