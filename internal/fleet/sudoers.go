package fleet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// LibexecDir is the root-owned directory the managed scripts are installed to.
// It must not be writable by the admin user: if it were, NOPASSWD on these
// paths would be a straight root escalation.
const LibexecDir = "/usr/local/libexec/umm"

// SudoersPath is the drop-in installed on each Mac.
const SudoersPath = "/etc/sudoers.d/umm"

// ManagedScripts are the only commands the sudoers drop-in grants NOPASSWD.
// They are installed root-owned (0755) under LibexecDir.
var ManagedScripts = []string{"install", "upgrade", "uninstall"}

// sudo usernames are conservative: sudoers has its own quoting rules and we do
// not want to render anything exotic into /etc/sudoers.d.
var sudoUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// RenderSudoers returns the drop-in granting user NOPASSWD for exactly the
// managed scripts under LibexecDir, and nothing else. The scripts are listed
// with absolute paths; a Cmnd_Alias keeps it readable.
func RenderSudoers(user string) (string, error) {
	if !sudoUserRe.MatchString(user) {
		return "", fmt.Errorf("refusing to render sudoers for unusual user name %q", user)
	}
	cmds := make([]string, 0, len(ManagedScripts))
	for _, s := range sortedCopy(ManagedScripts) {
		cmds = append(cmds, LibexecDir+"/"+s)
	}
	var b strings.Builder
	b.WriteString("# Managed by use-my-mac fleet. Do not edit by hand.\n")
	b.WriteString("# Grants NOPASSWD only for the root-owned umm install scripts.\n")
	fmt.Fprintf(&b, "Cmnd_Alias UMM_CMDS = %s\n", strings.Join(cmds, ", "))
	b.WriteString("Defaults!UMM_CMDS !requiretty\n")
	fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: UMM_CMDS\n", user)
	return b.String(), nil
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// scriptPath returns the absolute path of a managed script on the Mac.
func scriptPath(name string) string { return LibexecDir + "/" + name }

// CheckSudoers writes content to a private temp file and validates it with a
// visudo-style checker, mirroring the step bootstrap.sh runs on the Mac
// (`visudo -cf`). validator defaults to ["visudo", "-cf"]; the temp file path
// is appended as the final argument. It returns a non-nil error, with the
// checker's output, when the content is rejected. This lets the drop-in be
// dry-run before it is ever shipped.
func CheckSudoers(ctx context.Context, content string, validator ...string) error {
	if len(validator) == 0 {
		validator = []string{"visudo", "-cf"}
	}
	f, err := os.CreateTemp("", "umm-sudoers-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o440); err != nil {
		return err
	}
	args := append(append([]string(nil), validator[1:]...), f.Name())
	cmd := exec.CommandContext(ctx, validator[0], args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sudoers rejected: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
