package installer

import (
	_ "embed"
	"text/template"
)

//go:embed install.sh.tmpl
var installSrc string

//go:embed uninstall.sh
var uninstallScript string

var installTmpl = template.Must(template.New("install").Parse(installSrc))

type plistData struct{ Label, Root string }

var agentPlistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.Root}}/usr/local/bin/umm-agent</string>
    <string>run</string>
    <string>-config</string>
    <string>{{.Root}}/usr/local/etc/umm/agent.yaml</string>
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
  <string>{{.Root}}/usr/local/var/umm</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>StandardOutPath</key>
  <string>{{.Root}}/usr/local/var/log/umm/agent.log</string>
  <key>StandardErrorPath</key>
  <string>{{.Root}}/usr/local/var/log/umm/agent.log</string>
</dict>
</plist>
`))

var updaterPlistTmpl = template.Must(template.New("updater").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{.Label}}.updater</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.Root}}/usr/local/bin/umm-agent</string>
    <string>update</string>
    <string>-config</string>
    <string>{{.Root}}/usr/local/etc/umm/agent.yaml</string>
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
  <string>{{.Root}}/usr/local/var/log/umm-updater.log</string>
  <key>StandardErrorPath</key>
  <string>{{.Root}}/usr/local/var/log/umm-updater.log</string>
</dict>
</plist>
`))
