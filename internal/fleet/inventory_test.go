package fleet

import (
	"strings"
	"testing"
)

const sampleInventory = `
router: https://umm.example.com
bundle: ./dist/umm-agent-darwin-arm64
defaults:
  port: 22
  user: fleetadmin
  ssh_key: ~/.ssh/id_ed25519
  known_hosts: ~/.ssh/known_hosts
  max_tasks: 2
  priority: utility
  labels:
    region: hq
  runtimes:
    ffmpeg: ["/opt/homebrew/bin/ffmpeg"]
hosts:
  - host: mac-a.example.com
    labels:
      chip: m3-ultra
  - host: mac-b.example.com
    port: 2222
    user: otheradmin
    name: studio-b
    max_tasks: 4
    service_label: com.example.helper
  - host: 10.1.2.3
`

func TestParseAndResolve(t *testing.T) {
	inv, err := ParseInventory([]byte(sampleInventory))
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := inv.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 {
		t.Fatalf("got %d hosts, want 3", len(hosts))
	}
	// Sorted by host: 10.1.2.3, mac-a, mac-b
	a := hosts[1]
	if a.Host != "mac-a.example.com" {
		t.Fatalf("host[1]=%s", a.Host)
	}
	if a.Port != 22 || a.User != "fleetadmin" || a.MaxTasks != 2 || a.Priority != "utility" {
		t.Errorf("defaults not applied: %+v", a)
	}
	if a.Name != "mac-a.example.com" {
		t.Errorf("name default = %s", a.Name)
	}
	if a.Labels["region"] != "hq" || a.Labels["chip"] != "m3-ultra" {
		t.Errorf("labels merge wrong: %v", a.Labels)
	}
	if a.ServiceLabel != DefaultServiceLabel {
		t.Errorf("service label default = %s", a.ServiceLabel)
	}
	if len(a.Runtimes["ffmpeg"]) != 1 {
		t.Errorf("runtimes not inherited: %v", a.Runtimes)
	}
	b := hosts[2]
	if b.Host != "mac-b.example.com" || b.Port != 2222 || b.User != "otheradmin" {
		t.Errorf("host overrides wrong: %+v", b)
	}
	if b.Name != "studio-b" || b.MaxTasks != 4 || b.ServiceLabel != "com.example.helper" {
		t.Errorf("host overrides wrong: %+v", b)
	}
}

func TestResolveSelectSubset(t *testing.T) {
	inv, _ := ParseInventory([]byte(sampleInventory))
	hosts, err := inv.Resolve("studio-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Host != "mac-b.example.com" {
		t.Fatalf("subset by name failed: %+v", hosts)
	}
	if _, err := inv.Resolve("nope.example.com"); err == nil {
		t.Error("expected error selecting unknown host")
	}
}

func TestInventoryValidation(t *testing.T) {
	cases := map[string]string{
		"no hosts":      "router: https://x\nhosts: []\n",
		"missing host":  "hosts:\n  - port: 22\n    user: a\n",
		"missing user":  "hosts:\n  - host: a.example.com\n",
		"bad port":      "defaults: {user: a}\nhosts:\n  - host: a.example.com\n    port: 70000\n",
		"unknown field": "hosts:\n  - host: a.example.com\n    user: a\n    frobnicate: yes\n",
		"bad host":      "hosts:\n  - host: \"has space\"\n    user: a\n",
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			inv, err := ParseInventory([]byte(yml))
			if err != nil {
				return // rejected at parse (unknown field / no hosts)
			}
			if _, err := inv.Resolve(); err == nil {
				t.Errorf("%s: expected error", name)
			}
		})
	}
}

func TestDuplicateHost(t *testing.T) {
	yml := "defaults: {user: a}\nhosts:\n  - host: a.example.com\n  - host: a.example.com\n"
	inv, err := ParseInventory([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inv.Resolve(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("want duplicate error, got %v", err)
	}
}
