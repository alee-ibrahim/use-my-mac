package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"os"
)

// Secret holds a sensitive value (a sudo password or a router admin key). It is
// deliberately awkward to print: its String/Format render "<redacted>", so it
// can never leak into a log line or an error by accident. The raw bytes are
// reachable only through Bytes.
type Secret struct {
	b []byte
}

// NewSecret wraps a value. The caller's slice is copied.
func NewSecret(b []byte) Secret {
	c := make([]byte, len(b))
	copy(c, b)
	return Secret{b: c}
}

// Bytes returns the raw secret. Feed it to stdin; never to argv.
func (s Secret) Bytes() []byte { return s.b }

// Reader returns the secret as a one-line reader suitable for `sudo -S` stdin.
func (s Secret) Reader() *bytes.Reader {
	// sudo -S reads up to the first newline as the password.
	return bytes.NewReader(append(bytesClone(s.b), '\n'))
}

// Empty reports whether no secret is set.
func (s Secret) Empty() bool { return len(s.b) == 0 }

// String never reveals the secret.
func (s Secret) String() string {
	if len(s.b) == 0 {
		return ""
	}
	return "<redacted>"
}

// Format implements fmt.Formatter so %v, %s and %q all redact.
func (s Secret) Format(f fmt.State, verb rune) {
	switch verb {
	case 'q':
		fmt.Fprintf(f, "%q", s.String())
	default:
		fmt.Fprint(f, s.String())
	}
}

func bytesClone(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// SecretSource describes where a secret comes from. Exactly one of File or Env
// may be set; otherwise Prompt is used interactively.
type SecretSource struct {
	// File reads the whole file; a single trailing newline is trimmed.
	File string
	// Env is the NAME of an environment variable to read (never the value on
	// the command line).
	Env string
	// Prompt, with File and Env empty, reads from the terminal with echo off.
	Prompt bool
	// PromptLabel is shown when prompting.
	PromptLabel string
}

// prompter reads a line with echo disabled; overridable in tests.
var prompter = promptNoEcho

// Load resolves the secret. It returns an error, never a partial value, so a
// missing secret fails loudly instead of running sudo with an empty password.
func (src SecretSource) Load() (Secret, error) {
	set := 0
	if src.File != "" {
		set++
	}
	if src.Env != "" {
		set++
	}
	if set > 1 {
		return Secret{}, errors.New("give the secret from only one of a file or an environment variable")
	}
	switch {
	case src.File != "":
		b, err := os.ReadFile(src.File)
		if err != nil {
			return Secret{}, fmt.Errorf("read secret file: %w", err)
		}
		return NewSecret(trimOneNewline(b)), nil
	case src.Env != "":
		v, ok := os.LookupEnv(src.Env)
		if !ok {
			return Secret{}, fmt.Errorf("environment variable %s is not set", src.Env)
		}
		if v == "" {
			return Secret{}, fmt.Errorf("environment variable %s is empty", src.Env)
		}
		return NewSecret([]byte(v)), nil
	case src.Prompt:
		label := src.PromptLabel
		if label == "" {
			label = "Secret: "
		}
		b, err := prompter(label)
		if err != nil {
			return Secret{}, err
		}
		if len(b) == 0 {
			return Secret{}, errors.New("empty secret")
		}
		return NewSecret(b), nil
	}
	return Secret{}, errors.New("no secret source given")
}

func trimOneNewline(b []byte) []byte {
	return bytes.TrimRight(b, "\r\n")
}
