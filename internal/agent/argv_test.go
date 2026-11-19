package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubstituteArgs(t *testing.T) {
	wd := filepath.Join("w", "t1")
	got, err := SubstituteArgs([]string{"-i", "{in:src.mp4}", "-o={out:enc.mp4}", "{workdir}/x"}, wd,
		map[string]bool{"src.mp4": true}, map[string]bool{"enc.mp4": true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-i", filepath.Join(wd, "in", "src.mp4"), "-o=" + filepath.Join(wd, "out", "enc.mp4"), wd + "/x"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d: %q want %q", i, got[i], want[i])
		}
	}
	if _, err := SubstituteArgs([]string{"{in:nope}"}, wd, nil, nil); err == nil {
		t.Error("unknown input must fail")
	}
	if _, err := SubstituteArgs([]string{"{out:nope}"}, wd, nil, nil); err == nil {
		t.Error("unknown output must fail")
	}
}

func TestBuildArgv(t *testing.T) {
	rt := []string{"/opt/homebrew/bin/ffmpeg"}
	got := strings.Join(BuildArgv("darwin", "background", rt, []string{"-y"}), " ")
	if got != "/usr/sbin/taskpolicy -b /opt/homebrew/bin/ffmpeg -y" {
		t.Errorf("darwin background: %s", got)
	}
	got = strings.Join(BuildArgv("darwin", "utility", rt, nil), " ")
	if got != "/usr/sbin/taskpolicy -c utility /usr/bin/nice -n 10 /opt/homebrew/bin/ffmpeg" {
		t.Errorf("darwin utility: %s", got)
	}
	got = strings.Join(BuildArgv("linux", "background", rt, nil), " ")
	if got != "/usr/bin/nice -n 19 /opt/homebrew/bin/ffmpeg" {
		t.Errorf("linux: %s", got)
	}
	if got := BuildArgv("darwin", "none", rt, nil); len(got) != 1 {
		t.Errorf("none: %v", got)
	}
}

func TestTaskEnv(t *testing.T) {
	env, err := TaskEnv("/w", "tsk", 2, map[string]string{"AGENT": "1"}, map[string]string{"QUALITY": "high"})
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(env, "\n")
	for _, want := range []string{"HOME=/w", "UMM_ATTEMPT=2", "QUALITY=high", "AGENT=1", "UMM_OUT=" + filepath.Join("/w", "out")} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"PATH", "DYLD_INSERT_LIBRARIES", "LD_PRELOAD", "UMM_IN", "lower", "HOME"} {
		if _, err := TaskEnv("/w", "t", 1, nil, map[string]string{bad: "x"}); err == nil {
			t.Errorf("%s must be rejected", bad)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "a.yaml")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	c, err := LoadConfig(write("server: https://r.example/\nwork_dir: /tmp/w\nruntimes:\n  ffmpeg: [/opt/homebrew/bin/ffmpeg]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server != "https://r.example" || c.Priority != "adaptive" || c.PriorityIdle != 5*time.Minute || c.MaxTasks != 1 || c.Yield.UserCPUHigh != 50 {
		t.Fatalf("defaults: %+v", c)
	}
	bad := []string{
		"work_dir: /w\nruntimes: {x: [/bin/x]}\n",
		"server: s\nruntimes: {x: [/bin/x]}\n",
		"server: s\nwork_dir: /w\n",
		"server: s\nwork_dir: /w\nruntimes: {x: [relative/bin]}\n",
		"server: s\nwork_dir: /w\nruntimes: {\"a b\": [/bin/x]}\n",
		"server: s\nwork_dir: /w\npriority: turbo\nruntimes: {x: [/bin/x]}\n",
	}
	for i, b := range bad {
		if _, err := LoadConfig(write(b)); err == nil {
			t.Errorf("config %d should fail", i)
		}
	}
}
