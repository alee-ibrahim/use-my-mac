package installer

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

const testToken = "umm_ins_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func testParams() Params {
	return Params{
		Router: "https://umm.example.com", Token: testToken, Version: "v1.2.3",
		PublicKey:    "RWSi59qkCqNixPBPa16z3sPii3FP1cQ02xqfl5VL9IZjlQRbhGO7l2X9",
		ArtifactBase: "https://umm.example.com/i/" + testToken + "/releases/v1.2.3",
		Profile: api.InstallProfile{Name: "lab", Labels: map[string]string{"room": "lab-2"},
			Runtimes: map[string][]string{"ffmpeg": {"/opt/homebrew/bin/ffmpeg"}}},
	}
}

func shN(t *testing.T, script []byte) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = bytes.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

func TestRenderInstall(t *testing.T) {
	s, err := Render(testParams())
	if err != nil {
		t.Fatal(err)
	}
	shN(t, s)
	for _, want := range []string{
		"ROUTER='https://umm.example.com'", "TOKEN='" + testToken + "'", "VERSION='v1.2.3'",
		"LABEL='" + DefaultServiceLabel + "'", "PROTO='=https'", "R=''", "room: lab-2",
		"ffmpeg:\n    - /opt/homebrew/bin/ffmpeg", "osascript -l JavaScript", "main \"$@\"",
		"<string>" + DefaultServiceLabel + ".updater</string>",
		`parent_dirs "$R/usr/local" "$R/usr/local/bin" "$R/usr/local/etc" "$R/usr/local/var" "$R/usr/local/var/log"`,
		`(umask 022; install -d -m 0755 -o root -g wheel "$d")`,
	} {
		if !bytes.Contains(s, []byte(want)) {
			t.Errorf("script lacks %q", want)
		}
	}
	// The token is only ever in the TOKEN assignment and the artifact URL.
	if n := bytes.Count(s, []byte(testToken)); n != 2 {
		t.Errorf("token appears %d times, want 2", n)
	}
	// The token reaches the agent over stdin, never argv.
	if !bytes.Contains(s, []byte(`enrol -config "$ETC/agent.yaml" < "$T/token"`)) {
		t.Error("token is not handed to the agent over stdin")
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	bad := []func(p *Params){
		func(p *Params) { p.Router = "https://umm.example.com/'; rm -rf /" },
		func(p *Params) { p.Router = "https://umm.example.com/$(id)" },
		func(p *Params) { p.Router = "http://umm.example.com" }, // http without dev mode
		func(p *Params) { p.Token = "umm_ins_x'$(id)'AAAAAAAAAAAAAAAAAAAAAAAA" },
		func(p *Params) { p.Token = "umm_key_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" },
		func(p *Params) { p.Version = "v1.2.3'" },
		func(p *Params) { p.Version = "latest" },
		func(p *Params) { p.PublicKey = "RW'x" },
		func(p *Params) { p.ArtifactBase = "https://x.example.com/a b" },
		func(p *Params) { p.Root = "/tmp/a'b" },
		func(p *Params) { p.Profile.Labels = map[string]string{"room": "a\nUMM_EOF"} },
		func(p *Params) { p.Profile.Labels = map[string]string{"room": "'$(id)'"} },
		func(p *Params) { p.Profile.Labels = map[string]string{"a b": "x"} },
		func(p *Params) { p.Profile.Runtimes = map[string][]string{"x": {"relative/bin"}} },
		func(p *Params) { p.Profile.Runtimes = map[string][]string{"x": {"/bin/sh", "-c", "id; rm"}} },
		func(p *Params) { p.Profile.Runtimes = map[string][]string{"x": {"/bin/../bin/sh"}} },
		func(p *Params) { p.Profile.Runtimes = map[string][]string{"x y": {"/bin/sh"}} },
		func(p *Params) { p.Profile.Runtimes = nil },
		func(p *Params) { p.Profile.ServiceLabel = "com.example.x</string><string>" },
		func(p *Params) { p.Profile.ServiceLabel = "com.example.helper.updater" },
		func(p *Params) { p.Profile.Priority = "realtime" },
		func(p *Params) { p.Profile.Name = "Lab'" },
	}
	for i, mut := range bad {
		p := testParams()
		mut(&p)
		if _, err := Render(p); err == nil {
			t.Errorf("case %d: rendered a script from bad input %+v", i, p)
		}
	}
	// http is fine in dev mode.
	p := testParams()
	p.Router, p.ArtifactBase, p.AllowHTTP = "http://127.0.0.1:8080", "http://127.0.0.1:8080/i/"+testToken+"/releases/v1.2.3", true
	s, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(s, []byte("PROTO='=https,http'")) {
		t.Error("dev mode should allow http downloads")
	}
}

func TestErrorAndUninstallScripts(t *testing.T) {
	e := RenderError("link expired'; rm -rf / #$(id)")
	shN(t, e)
	if bytes.Contains(e, []byte("$(")) || bytes.Count(e, []byte("'")) != 4 {
		t.Errorf("error message not sanitised:\n%s", e)
	}
	u, err := RenderUninstall("")
	if err != nil {
		t.Fatal(err)
	}
	shN(t, u)
	if _, err := RenderUninstall("/tmp/x'y"); err == nil {
		t.Error("bad root accepted")
	}
	if runtime.GOOS != "windows" {
		out, err := exec.Command("sh", "-c", `printf '%s' "$1" | sh`, "sh", string(e)).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "use-my-mac: error: link expired") {
			t.Errorf("error script: err=%v out=%s", err, out)
		}
	}
}

func TestCommand(t *testing.T) {
	if got := Command("https://umm.example.com", testToken); got != "curl -fsSL https://umm.example.com/i/"+testToken+" | sudo sh" {
		t.Error(got)
	}
}

// TestVerifyJS runs the embedded minisign verifier under node (the same
// JavaScript osascript runs on the Mac) against signatures made by the Go
// signer.
func TestVerifyJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js := filepath.Join(t.TempDir(), "verify.js")
	os.WriteFile(js, []byte(verifyJS), 0o600)
	pub, sk, _ := selfupdate.GenerateKey()
	other, _, _ := selfupdate.GenerateKey()
	line := func(p *selfupdate.PublicKey) string {
		l := strings.Split(strings.TrimSpace(p.String()), "\n")
		return l[len(l)-1]
	}
	sums := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  umm-agent-darwin-arm64.tar.gz\n")
	tc := selfupdate.TrustedComment("v1.2.3")
	sig := sk.Sign(sums, tc)
	run := func(key, comment string, msg, sig []byte) (string, error) {
		out, err := exec.Command(node, js, key, comment, hex.EncodeToString(msg), hex.EncodeToString(sig)).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run(line(pub), tc, sums, sig); err != nil || out != "ok" {
		t.Fatalf("valid signature rejected: %v %s", err, out)
	}
	tampered := append([]byte{}, sums...)
	tampered[0] = '1'
	forged := []byte(strings.Replace(string(sig), "trusted comment: "+tc, "trusted comment: "+selfupdate.TrustedComment("v9.9.9"), 1))
	for name, c := range map[string]struct {
		key, tc  string
		msg, sig []byte
	}{
		"tampered file":        {line(pub), tc, tampered, sig},
		"other key":            {line(other), tc, sums, sig},
		"replayed version":     {line(pub), selfupdate.TrustedComment("v9.9.9"), sums, sig},
		"forged comment":       {line(pub), selfupdate.TrustedComment("v9.9.9"), sums, forged},
		"no signature":         {line(pub), tc, sums, []byte{}},
		"garbage signature":    {line(pub), tc, sums, []byte("untrusted comment: x\nAAAA\ntrusted comment: y\nAAAA\n")},
		"signature of another": {line(pub), tc, sums, sk.Sign([]byte("other"), tc)},
	} {
		if out, err := run(c.key, c.tc, c.msg, c.sig); err == nil || out == "ok" {
			t.Errorf("%s: accepted (%s)", name, out)
		}
	}
}
