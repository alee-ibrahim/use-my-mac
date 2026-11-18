// Package fleet drives SSH-based installation of the umm-agent across a set of
// Macs from an admin workstation (Windows, macOS or Linux). It never runs on a
// Mac or in the router, and agents never receive SSH keys.
//
// The entry point is an Inventory (YAML) describing the hosts, connection
// defaults and the router URL. Runner turns an Inventory into per-host results
// for the bootstrap/install/upgrade/uninstall/status actions.
package fleet

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Inventory is the YAML file an admin maintains. It holds no secrets: the SSH
// key is referenced by path (or taken from ssh-agent), and the sudo password
// and router admin key are supplied separately at run time from a file or an
// environment variable.
type Inventory struct {
	// Router is the base URL of the umm-server, e.g. https://umm.example.com.
	Router string `yaml:"router"`
	// Bundle is a directory holding the agent binary ("umm-agent"), the
	// LaunchDaemon plist and (optionally) an agent.yaml template to copy to
	// each Mac. It can be overridden per run with -bundle.
	Bundle string `yaml:"bundle"`

	Defaults HostDefaults `yaml:"defaults"`
	Hosts    []Host       `yaml:"hosts"`
}

// HostDefaults are applied to every host unless the host overrides them.
type HostDefaults struct {
	Port         int                 `yaml:"port"`
	User         string              `yaml:"user"`
	SSHKey       string              `yaml:"ssh_key"`
	KnownHosts   string              `yaml:"known_hosts"`
	MaxTasks     int                 `yaml:"max_tasks"`
	Priority     string              `yaml:"priority"`
	Labels       map[string]string   `yaml:"labels"`
	Runtimes     map[string][]string `yaml:"runtimes"`
	ServiceLabel string              `yaml:"service_label"`
}

// Host is one Mac to manage.
type Host struct {
	// Host is the DNS name or IP to connect to (required).
	Host string `yaml:"host"`
	// Port defaults to the inventory default, then 22.
	Port int `yaml:"port"`
	// User is the admin login on the Mac that holds an SSH authorized key and
	// can sudo (required, here or in defaults).
	User string `yaml:"user"`
	// SSHKey overrides the default private-key path for this host.
	SSHKey string `yaml:"ssh_key"`
	// Name is the agent name the Mac registers under (defaults to Host).
	Name string `yaml:"name"`
	// MaxTasks, Priority, Labels and Runtimes override the agent config
	// defaults for this host.
	MaxTasks     int                 `yaml:"max_tasks"`
	Priority     string              `yaml:"priority"`
	Labels       map[string]string   `yaml:"labels"`
	Runtimes     map[string][]string `yaml:"runtimes"`
	ServiceLabel string              `yaml:"service_label"`
}

// DefaultServiceLabel is the LaunchDaemon label used when none is configured.
// It matches the plist shipped in deploy/mac. A neutral label (e.g.
// "com.example.helper") can be set per host or in defaults for a quiet install.
const DefaultServiceLabel = "io.github.alee-ibrahim.umm-agent"

// ResolvedHost is a host with defaults merged in and validated.
type ResolvedHost struct {
	Host         string
	Port         int
	User         string
	SSHKey       string
	KnownHosts   string
	Name         string
	MaxTasks     int
	Priority     string
	Labels       map[string]string
	Runtimes     map[string][]string
	ServiceLabel string
}

// Addr is the "host:port" dial target.
func (h ResolvedHost) Addr() string { return net.JoinHostPort(h.Host, strconv.Itoa(h.Port)) }

var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.\-]{0,253}$`)

// LoadInventory reads and validates an inventory file.
func LoadInventory(path string) (*Inventory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseInventory(b)
}

// ParseInventory parses inventory YAML. Unknown fields are rejected so typos
// surface instead of being silently ignored.
func ParseInventory(b []byte) (*Inventory, error) {
	var inv Inventory
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&inv); err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	if len(inv.Hosts) == 0 {
		return nil, errors.New("inventory: no hosts")
	}
	return &inv, nil
}

// Resolve merges defaults into every host and validates. If only is non-empty
// it selects just those hosts (matched by host or name).
func (inv *Inventory) Resolve(only ...string) ([]ResolvedHost, error) {
	want := map[string]bool{}
	for _, o := range only {
		want[o] = true
	}
	var out []ResolvedHost
	seen := map[string]int{}
	d := inv.Defaults
	for i, h := range inv.Hosts {
		if strings.TrimSpace(h.Host) == "" {
			return nil, fmt.Errorf("host %d: host is required", i)
		}
		if !hostnameRe.MatchString(h.Host) && net.ParseIP(h.Host) == nil {
			return nil, fmt.Errorf("host %q: not a valid hostname or IP", h.Host)
		}
		r := ResolvedHost{
			Host:         h.Host,
			Port:         firstInt(h.Port, d.Port, 22),
			User:         firstStr(h.User, d.User),
			SSHKey:       firstStr(h.SSHKey, d.SSHKey),
			KnownHosts:   d.KnownHosts,
			Name:         firstStr(h.Name, h.Host),
			MaxTasks:     firstInt(h.MaxTasks, d.MaxTasks, 1),
			Priority:     firstStr(h.Priority, d.Priority, "background"),
			Labels:       mergeLabels(d.Labels, h.Labels),
			Runtimes:     mergeRuntimes(d.Runtimes, h.Runtimes),
			ServiceLabel: firstStr(h.ServiceLabel, d.ServiceLabel, DefaultServiceLabel),
		}
		if r.User == "" {
			return nil, fmt.Errorf("host %q: user is required (set it on the host or in defaults)", h.Host)
		}
		if r.Port < 1 || r.Port > 65535 {
			return nil, fmt.Errorf("host %q: port %d out of range", h.Host, r.Port)
		}
		if !serviceLabelRe.MatchString(r.ServiceLabel) {
			return nil, fmt.Errorf("host %q: service_label %q is invalid", h.Host, r.ServiceLabel)
		}
		key := r.Addr()
		if seen[key] > 0 {
			return nil, fmt.Errorf("duplicate host %s", key)
		}
		seen[key]++
		if len(want) > 0 && !want[h.Host] && !want[r.Name] {
			continue
		}
		out = append(out, r)
	}
	if len(want) > 0 {
		for o := range want {
			found := false
			for _, r := range out {
				if r.Host == o || r.Name == o {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("host %q is not in the inventory", o)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no hosts selected")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

var serviceLabelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.\-]{1,200}$`)

func firstStr(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func firstInt(v ...int) int {
	for _, i := range v {
		if i > 0 {
			return i
		}
	}
	return 0
}

func mergeLabels(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func mergeRuntimes(base, over map[string][]string) map[string][]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := map[string][]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}
