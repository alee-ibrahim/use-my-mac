//go:build integration

// Integration test for the SSH-based fleet installer. It runs against a real
// OpenSSH server (an Alpine container, see the test harness) that stands in for
// a Mac: root-capable via sudo, with GNU coreutils and visudo, but no macOS
// tools, so it exercises the SSH transport, sftp staging, the sudo-password-
// over-stdin path, real `visudo -cf` validation, and NOPASSWD scoping end to
// end. The mac-only parts of install/upgrade are covered by the unit tests.
//
// Driven by environment variables (set by the harness); the test skips when
// they are absent:
//
//	UMM_TEST_SSH_ADDR   host:port of the sshd container
//	UMM_TEST_SSH_USER   admin login (sudoer)
//	UMM_TEST_SSH_KEY    path to the private key authorised on the server
//	UMM_TEST_SUDO_PASS  the admin's sudo password
package fleet

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func itestHost(t *testing.T) (ResolvedHost, SSHOptions) {
	t.Helper()
	addr := os.Getenv("UMM_TEST_SSH_ADDR")
	if addr == "" {
		t.Skip("set UMM_TEST_SSH_ADDR to run the SSH integration test")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("bad UMM_TEST_SSH_ADDR %q: %v", addr, err)
	}
	port := 22
	if p, err := net.LookupPort("tcp", portStr); err == nil {
		port = p
	}
	h := ResolvedHost{
		Host: host, Port: port, User: os.Getenv("UMM_TEST_SSH_USER"), Name: "itest-mac",
		MaxTasks: 1, Priority: "background", ServiceLabel: DefaultServiceLabel,
		Runtimes: map[string][]string{"ffmpeg": {"/opt/homebrew/bin/ffmpeg"}},
	}
	signers, err := LoadSigners(os.Getenv("UMM_TEST_SSH_KEY"))
	if err != nil {
		t.Fatalf("load signers: %v", err)
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	cb, _, err := HostKeyChecker(kh, true) // accept-new (TOFU)
	if err != nil {
		t.Fatalf("host key checker: %v", err)
	}
	t.Cleanup(func() {
		if b, _ := os.ReadFile(kh); len(b) == 0 {
			t.Error("known_hosts was not populated by accept-new TOFU")
		}
	})
	return h, SSHOptions{Signers: signers, HostKey: cb, Timeout: 15 * time.Second}
}

func TestIntegrationTransport(t *testing.T) {
	h, opt := itestHost(t)
	ctx := context.Background()
	target, err := Dial(ctx, h, opt)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer target.Close()

	dir, err := target.MkdirTemp(ctx)
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	if !strings.HasPrefix(dir, "/") {
		t.Fatalf("mkdirtemp returned %q", dir)
	}

	// Upload with an explicit mode and read it back.
	content := []byte("hello from the fleet installer\n")
	remote := dir + "/probe"
	if err := target.Upload(ctx, remote, content, 0o640); err != nil {
		t.Fatalf("upload: %v", err)
	}
	res, err := target.Run(ctx, "cat "+shellQuote(remote), nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("cat: %v exit=%d %s", err, res.ExitCode, res.Stderr)
	}
	if res.Stdout != string(content) {
		t.Errorf("round-trip mismatch: %q", res.Stdout)
	}
	res, _ = target.Run(ctx, "stat -c %a "+shellQuote(remote), nil)
	if got := strings.TrimSpace(res.Stdout); got != "640" {
		t.Errorf("uploaded mode = %q, want 640", got)
	}

	// Exit codes propagate.
	res, err = target.Run(ctx, "exit 3", nil)
	if err != nil {
		t.Fatalf("run exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

func TestIntegrationBootstrapAndNOPASSWD(t *testing.T) {
	h, opt := itestHost(t)
	ctx := context.Background()
	pass := NewSecret([]byte(os.Getenv("UMM_TEST_SUDO_PASS")))

	r := NewRunner(Options{SudoPassword: pass, SSH: opt})

	// 1) bootstrap: installs root-owned scripts + a validated sudoers drop-in.
	res := r.Run(ctx, []ResolvedHost{h}, ActionBootstrap)
	if len(res) != 1 || !res[0].OK() {
		t.Fatalf("bootstrap failed: %+v (err=%v)", res[0], res[0].Err)
	}
	t.Logf("bootstrap: %s", res[0].Message)

	// 2) prove NOPASSWD works: uninstall runs via `sudo -n` with no password.
	rNoPass := NewRunner(Options{SSH: opt}) // deliberately no sudo password
	res = rNoPass.Run(ctx, []ResolvedHost{h}, ActionUninstall)
	if !res[0].OK() {
		t.Fatalf("uninstall via sudo -n failed (NOPASSWD not in effect): %+v err=%v", res[0], res[0].Err)
	}
	t.Logf("uninstall (sudo -n, no password): %s", res[0].Message)

	// 3) prove scope is narrow: a command NOT in the allowlist is refused by
	//    `sudo -n` (it would need a password).
	target, err := Dial(ctx, h, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	out, err := target.Run(ctx, "sudo -n /bin/true 2>&1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode == 0 {
		t.Error("sudo -n /bin/true should be refused: the NOPASSWD grant is too broad")
	}
	t.Logf("non-allowlisted sudo -n refused as expected (exit %d)", out.ExitCode)
}
