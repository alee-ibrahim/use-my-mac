package fleet

import (
	"context"
	"strings"
	"testing"
)

func TestRenderSudoers(t *testing.T) {
	out, err := RenderSudoers("fleetadmin")
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the three managed scripts, root-owned paths, NOPASSWD, scoped.
	for _, want := range []string{
		"Cmnd_Alias UMM_CMDS = /usr/local/libexec/umm/install, /usr/local/libexec/umm/uninstall, /usr/local/libexec/umm/upgrade",
		"fleetadmin ALL=(root) NOPASSWD: UMM_CMDS",
		"Defaults!UMM_CMDS !requiretty",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered sudoers missing %q\n---\n%s", want, out)
		}
	}
	// Must not grant anything broader than the scripts.
	if strings.Contains(out, "ALL=(ALL)") || strings.Contains(out, "NOPASSWD: ALL") {
		t.Errorf("sudoers too broad:\n%s", out)
	}
}

func TestRenderSudoersRejectsBadUser(t *testing.T) {
	for _, u := range []string{"", "root; rm -rf /", "a b", "../evil", "User@Name"} {
		if _, err := RenderSudoers(u); err == nil {
			t.Errorf("expected rejection of user %q", u)
		}
	}
}

// TestCheckSudoersFlow exercises the render -> write -> validate flow with a
// stub validator (visudo is not guaranteed in this environment).
func TestCheckSudoersFlow(t *testing.T) {
	content, err := RenderSudoers("fleetadmin")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Valid: a checker that confirms the alias is present and exits 0.
	if err := CheckSudoers(ctx, content, "sh", "-c", `grep -q "UMM_CMDS" "$0"`); err != nil {
		t.Errorf("valid content rejected: %v", err)
	}
	// Invalid: a checker that always fails, and whose output is surfaced.
	err = CheckSudoers(ctx, content, "sh", "-c", `echo "parse error near line 1" >&2; exit 1`)
	if err == nil {
		t.Fatal("expected validation failure")
	}
	if !strings.Contains(err.Error(), "parse error") {
		t.Errorf("validator output not surfaced: %v", err)
	}
}
