// Package runtimes implements managed runtimes: signed packages (a tool such
// as ffmpeg, built for a platform) that the router publishes in a catalog and
// that agents download, verify and install into their own prefix when a task
// needs them. No Homebrew, no sudo, no prompt on the Mac.
//
// Trust: the router only relays. A package is installed only when its
// manifest carries a minisign signature from a key the agent trusts (pinned
// in the root-owned agent.yaml, or the release key built into the agent,
// which already decides what the agent itself runs) and agent.yaml allows
// the package. The manifest pins the download's sha256, so where the bytes
// come from (router storage or any URL) does not matter.
package runtimes

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Limits.
const (
	MaxManifestBytes = 64 << 10
	MaxCommands      = 64
	MaxArtifacts     = 16
	// DefaultMaxBytes caps one download when the manifest sets no size.
	DefaultMaxBytes = 2 << 30
)

var (
	// PackageNameRe: lower-case package names.
	PackageNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	versionRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	commandRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	platformRe    = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	fileRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
)

// TrustedComment is what a package signature's trusted comment must say. It
// binds the signature to one package name and version, so it can't be
// replayed for another package (or confused with a release signature).
func TrustedComment(name, version string) string {
	return "use-my-mac runtime " + name + " " + version
}

// relPath checks a path relative to the package root.
func relPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.ContainsRune(p, 0) {
		return fmt.Errorf("path %q must be relative", p)
	}
	if c := path.Clean(p); c != p || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("path %q must be a clean path inside the package", p)
	}
	return nil
}

// Validate checks a manifest.
func Validate(p *api.RuntimePackage) error {
	if !PackageNameRe.MatchString(p.Name) {
		return fmt.Errorf("package name %q is invalid (lower-case letters, digits and ._-)", p.Name)
	}
	if !versionRe.MatchString(p.Version) {
		return fmt.Errorf("package %s: version %q is invalid", p.Name, p.Version)
	}
	if len(p.Description) > 500 {
		return fmt.Errorf("package %s: description too long", p.Name)
	}
	if len(p.Commands) == 0 || len(p.Commands) > MaxCommands {
		return fmt.Errorf("package %s: needs 1..%d commands", p.Name, MaxCommands)
	}
	for c, argv := range p.Commands {
		if !commandRe.MatchString(c) {
			return fmt.Errorf("package %s: command name %q is invalid", p.Name, c)
		}
		if len(argv) == 0 || len(argv) > 64 {
			return fmt.Errorf("package %s: command %s needs a program", p.Name, c)
		}
		if err := relPath(argv[0]); err != nil {
			return fmt.Errorf("package %s: command %s: %w", p.Name, c, err)
		}
		for _, a := range argv[1:] {
			if len(a) > 4096 || strings.ContainsRune(a, 0) {
				return fmt.Errorf("package %s: command %s: bad argument", p.Name, c)
			}
		}
	}
	for _, d := range p.Path {
		if err := relPath(d); err != nil {
			return fmt.Errorf("package %s: path: %w", p.Name, err)
		}
	}
	if len(p.Artifacts) == 0 || len(p.Artifacts) > MaxArtifacts {
		return fmt.Errorf("package %s: needs 1..%d artifacts", p.Name, MaxArtifacts)
	}
	seen := map[string]bool{}
	for i, a := range p.Artifacts {
		where := fmt.Sprintf("package %s: artifact %d", p.Name, i)
		if !platformRe.MatchString(a.OS) || !platformRe.MatchString(a.Arch) {
			return fmt.Errorf("%s: os/arch %q/%q are invalid", where, a.OS, a.Arch)
		}
		if seen[a.OS+"/"+a.Arch] {
			return fmt.Errorf("%s: duplicate platform %s/%s", where, a.OS, a.Arch)
		}
		seen[a.OS+"/"+a.Arch] = true
		if (a.URL == "") == (a.Blob == "") {
			return fmt.Errorf("%s: set exactly one of url or blob", where)
		}
		if a.URL != "" {
			u, err := url.Parse(a.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return fmt.Errorf("%s: url must be http(s)", where)
			}
		}
		if a.Blob != "" && (len(a.Blob) > 512 || strings.Contains(a.Blob, "..") || strings.HasPrefix(a.Blob, "/")) {
			return fmt.Errorf("%s: blob key is invalid", where)
		}
		if b, err := hex.DecodeString(a.SHA256); err != nil || len(b) != 32 {
			return fmt.Errorf("%s: sha256 must be 64 hex characters", where)
		}
		if a.Size < 0 {
			return fmt.Errorf("%s: negative size", where)
		}
		switch a.Format {
		case api.FormatTarGz, api.FormatZip:
			if a.File != "" {
				return fmt.Errorf("%s: file is only for format %q", where, api.FormatFile)
			}
		case api.FormatFile:
			if !fileRe.MatchString(a.File) {
				return fmt.Errorf("%s: format file needs a plain file name", where)
			}
		default:
			return fmt.Errorf("%s: format must be tar.gz, zip or file", where)
		}
	}
	return nil
}

// Parse decodes and validates a manifest. Unknown fields are rejected so a
// typo can't silently drop a constraint.
func Parse(manifest []byte) (*api.RuntimePackage, error) {
	if len(manifest) > MaxManifestBytes {
		return nil, errors.New("manifest too large")
	}
	var p api.RuntimePackage
	dec := json.NewDecoder(bytes.NewReader(manifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if dec.More() {
		return nil, errors.New("manifest: trailing data")
	}
	if err := Validate(&p); err != nil {
		return nil, err
	}
	for i := range p.Artifacts {
		p.Artifacts[i].SHA256 = strings.ToLower(p.Artifacts[i].SHA256)
	}
	return &p, nil
}

// CheckSignature parses the manifest and checks that sig is well formed and
// made for this package's name and version. It does not check who signed
// it (the router does not decide trust; agents do). It returns the key ID.
func CheckSignature(manifest, sig []byte) (*api.RuntimePackage, string, error) {
	p, err := Parse(manifest)
	if err != nil {
		return nil, "", err
	}
	s, err := selfupdate.ParseSignature(sig)
	if err != nil {
		return nil, "", err
	}
	if s.TrustedComment != TrustedComment(p.Name, p.Version) {
		return nil, "", fmt.Errorf("signature is for %q, not %q", s.TrustedComment, TrustedComment(p.Name, p.Version))
	}
	return p, selfupdate.KeyIDString(s.KeyID), nil
}

// Verify checks the manifest's signature against the trusted keys and
// returns the package.
func Verify(keys []*selfupdate.PublicKey, manifest, sig []byte) (*api.RuntimePackage, error) {
	p, _, err := CheckSignature(manifest, sig)
	if err != nil {
		return nil, err
	}
	s, _ := selfupdate.ParseSignature(sig)
	for _, k := range keys {
		if k.KeyID == s.KeyID {
			if err := selfupdate.Verify(k, manifest, s); err != nil {
				return nil, err
			}
			return p, nil
		}
	}
	return nil, fmt.Errorf("package %s is signed by key %s, which this Mac does not trust", p.Name, selfupdate.KeyIDString(s.KeyID))
}

// Sign signs a manifest (CLI and tests). The manifest must be valid.
func Sign(sk *selfupdate.SecretKey, manifest []byte) ([]byte, error) {
	p, err := Parse(manifest)
	if err != nil {
		return nil, err
	}
	return sk.Sign(manifest, TrustedComment(p.Name, p.Version)), nil
}

// ArtifactFor returns the index of the artifact for a platform, or -1.
func ArtifactFor(p *api.RuntimePackage, goos, goarch string) int {
	for i, a := range p.Artifacts {
		if a.OS == goos && a.Arch == goarch {
			return i
		}
	}
	return -1
}

// PathDirs returns the package's PATH directories (relative).
func PathDirs(p *api.RuntimePackage) []string {
	if len(p.Path) > 0 {
		return append([]string(nil), p.Path...)
	}
	seen := map[string]bool{}
	var out []string
	for _, argv := range p.Commands {
		d := path.Dir(argv[0])
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// Allowed reports whether name matches an allow list ("*" = everything).
func Allowed(allow []string, name string) bool {
	for _, a := range allow {
		if a == "*" || a == name {
			return true
		}
	}
	return false
}
