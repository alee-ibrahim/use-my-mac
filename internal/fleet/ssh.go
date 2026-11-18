package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Result is the outcome of a remote command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Target is one Mac reached over SSH. It is an interface so the orchestrator
// can be exercised with a fake in unit tests while the integration test uses a
// real SSH server.
type Target interface {
	// Run executes cmd on the remote shell. stdin may be nil. A non-zero exit
	// is returned in Result (not as an error); err is for transport failures.
	Run(ctx context.Context, cmd string, stdin io.Reader) (Result, error)
	// Upload writes data to remotePath with the given mode, creating parent
	// directories as needed.
	Upload(ctx context.Context, remotePath string, data []byte, mode os.FileMode) error
	// MkdirTemp creates a fresh 0700 directory and returns its path.
	MkdirTemp(ctx context.Context) (string, error)
	Close() error
}

// SSHOptions configure how targets are dialed.
type SSHOptions struct {
	Signers       []ssh.Signer
	HostKey       ssh.HostKeyCallback
	Timeout       time.Duration
	KnownHostsAdd func(hostport string, key ssh.PublicKey) error // for TOFU accept-new
}

// LoadSigners builds the auth signers: each given private-key file, plus any
// keys offered by a running ssh-agent (SSH_AUTH_SOCK). All signers are tried
// against every host, so a mix of per-host keys works transparently.
func LoadSigners(keyPaths ...string) ([]ssh.Signer, error) {
	var signers []ssh.Signer
	seen := map[string]bool{}
	for _, keyPath := range keyPaths {
		if keyPath == "" || seen[keyPath] {
			continue
		}
		seen[keyPath] = true
		b, err := os.ReadFile(expandHome(keyPath))
		if err != nil {
			return nil, fmt.Errorf("read ssh key: %w", err)
		}
		s, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("parse ssh key %s: %w (encrypted keys must be added to ssh-agent)", keyPath, err)
		}
		signers = append(signers, s)
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			if ag := agent.NewClient(conn); ag != nil {
				if as, err := ag.Signers(); err == nil {
					signers = append(signers, as...)
				}
			}
		}
	}
	if len(signers) == 0 {
		return nil, errors.New("no SSH key: set ssh_key in the inventory, pass -ssh-key, or load a key into ssh-agent")
	}
	return signers, nil
}

// knownHostsPath returns the known_hosts path, defaulting to ~/.ssh/known_hosts.
func knownHostsPath(p string) string {
	if p == "" {
		p = "~/.ssh/known_hosts"
	}
	return expandHome(p)
}

// HostKeyChecker returns a callback that verifies host keys against known_hosts.
// When acceptNew is true, an unknown host is trusted on first use and appended
// to the file (TOFU); a key that conflicts with a recorded one is always
// rejected. The returned addFn may be nil when acceptNew is false.
func HostKeyChecker(knownHosts string, acceptNew bool) (ssh.HostKeyCallback, func(string, ssh.PublicKey) error, error) {
	kh := knownHostsPath(knownHosts)
	if err := os.MkdirAll(filepath.Dir(kh), 0o700); err == nil {
		// ensure the file exists so knownhosts.New does not fail
		if _, statErr := os.Stat(kh); os.IsNotExist(statErr) {
			_ = os.WriteFile(kh, nil, 0o600)
		}
	}
	base, err := knownhosts.New(kh)
	if err != nil {
		return nil, nil, fmt.Errorf("known_hosts %s: %w", kh, err)
	}
	if !acceptNew {
		return base, nil, nil
	}
	var mu sync.Mutex
	add := func(hostport string, key ssh.PublicKey) error {
		mu.Lock()
		defer mu.Unlock()
		f, err := os.OpenFile(kh, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		line := knownhosts.Line([]string{knownhosts.Normalize(hostport)}, key)
		_, err = fmt.Fprintln(f, line)
		return err
	}
	cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := base(hostname, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			// Unknown host: trust on first use and record it.
			return add(hostname, key)
		}
		return err // mismatch with a recorded key, or other error
	}
	return cb, add, nil
}

// SSHTarget is a live connection to one Mac.
type SSHTarget struct {
	client *ssh.Client
	sftp   *sftp.Client
	user   string
}

// Dial opens an SSH connection to host.
func Dial(ctx context.Context, h ResolvedHost, opt SSHOptions) (*SSHTarget, error) {
	if opt.HostKey == nil {
		return nil, errors.New("no host key callback")
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:            h.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(opt.Signers...)},
		HostKeyCallback: opt.HostKey,
		Timeout:         timeout,
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", h.Addr())
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, h.Addr(), cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	return &SSHTarget{client: client, user: h.User}, nil
}

// Run executes cmd, optionally feeding stdin.
func (t *SSHTarget) Run(ctx context.Context, cmd string, stdin io.Reader) (Result, error) {
	sess, err := t.client.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &errb
	if stdin != nil {
		sess.Stdin = stdin
	}
	done := make(chan error, 1)
	if err := sess.Start(cmd); err != nil {
		return Result{}, err
	}
	go func() { done <- sess.Wait() }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return Result{Stdout: out.String(), Stderr: errb.String()}, ctx.Err()
	case werr := <-done:
		res := Result{Stdout: out.String(), Stderr: errb.String()}
		if werr == nil {
			return res, nil
		}
		var ee *ssh.ExitError
		if errors.As(werr, &ee) {
			res.ExitCode = ee.ExitStatus()
			return res, nil
		}
		return res, werr
	}
}

func (t *SSHTarget) ensureSFTP() (*sftp.Client, error) {
	if t.sftp != nil {
		return t.sftp, nil
	}
	c, err := sftp.NewClient(t.client)
	if err != nil {
		return nil, err
	}
	t.sftp = c
	return c, nil
}

// Upload writes data to remotePath with mode.
func (t *SSHTarget) Upload(ctx context.Context, remotePath string, data []byte, mode os.FileMode) error {
	c, err := t.ensureSFTP()
	if err != nil {
		return err
	}
	if dir := path.Dir(remotePath); dir != "." && dir != "/" {
		_ = c.MkdirAll(dir)
	}
	f, err := c.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return c.Chmod(remotePath, mode)
}

// MkdirTemp makes a private staging directory under the user's home.
func (t *SSHTarget) MkdirTemp(ctx context.Context) (string, error) {
	// mktemp is available on macOS and Linux; -d makes a 0700 directory.
	res, err := t.Run(ctx, "mktemp -d 2>/dev/null || mktemp -d -t umm", nil)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("mktemp: %s", strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Close shuts the connection down.
func (t *SSHTarget) Close() error {
	if t.sftp != nil {
		t.sftp.Close()
	}
	if t.client != nil {
		return t.client.Close()
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}
