package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretRedaction(t *testing.T) {
	s := NewSecret([]byte("hunter2"))
	if got := s.String(); got != "<redacted>" {
		t.Errorf("String()=%q", got)
	}
	// Every common format verb must redact.
	for _, f := range []string{fmt.Sprintf("%v", s), fmt.Sprintf("%s", s), fmt.Sprintf("%q", s), fmt.Sprintf("%+v", s)} {
		if strings.Contains(f, "hunter2") {
			t.Errorf("secret leaked via format: %q", f)
		}
	}
	// The raw value is only reachable through Bytes.
	if string(s.Bytes()) != "hunter2" {
		t.Errorf("Bytes()=%q", s.Bytes())
	}
	// Reader yields the value plus a single newline (for `sudo -S`).
	buf := make([]byte, 64)
	n, _ := s.Reader().Read(buf)
	if string(buf[:n]) != "hunter2\n" {
		t.Errorf("Reader()=%q", buf[:n])
	}
}

func TestSecretSourceFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pw")
	if err := os.WriteFile(p, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := SecretSource{File: p}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Bytes()) != "s3cr3t" { // one trailing newline trimmed
		t.Errorf("file secret=%q", s.Bytes())
	}
}

func TestSecretSourceEnv(t *testing.T) {
	t.Setenv("UMM_TEST_PW", "envpass")
	s, err := SecretSource{Env: "UMM_TEST_PW"}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Bytes()) != "envpass" {
		t.Errorf("env secret=%q", s.Bytes())
	}
	if _, err := (SecretSource{Env: "UMM_TEST_DOES_NOT_EXIST"}).Load(); err == nil {
		t.Error("expected error for unset env var")
	}
}

func TestSecretSourceConflict(t *testing.T) {
	if _, err := (SecretSource{File: "a", Env: "B"}).Load(); err == nil {
		t.Error("expected error when both file and env are set")
	}
	if _, err := (SecretSource{}).Load(); err == nil {
		t.Error("expected error when no source is set")
	}
}

func TestSecretSourcePrompt(t *testing.T) {
	old := prompter
	defer func() { prompter = old }()
	prompter = func(label string) ([]byte, error) { return []byte("typed-pass"), nil }
	s, err := SecretSource{Prompt: true, PromptLabel: "pw: "}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Bytes()) != "typed-pass" {
		t.Errorf("prompt secret=%q", s.Bytes())
	}
}
