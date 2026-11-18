package selfupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Release artifact names.
const (
	SumsFile = "SHA256SUMS"
	SigFile  = "SHA256SUMS.minisig"
)

// BundleName is the agent bundle for a platform, e.g. umm-agent-darwin-arm64.
func BundleName(goos, goarch string) string { return "umm-agent-" + goos + "-" + goarch }

// BundleFile is the tarball name in a release.
func BundleFile(goos, goarch string) string { return BundleName(goos, goarch) + ".tar.gz" }

// ArtifactAllowed reports whether name is a file the router may relay.
func ArtifactAllowed(name string) bool {
	switch name {
	case SumsFile, SigFile, BundleFile("darwin", "arm64"):
		return true
	}
	return false
}

// MaxArtifactBytes bounds every download (the bundle is ~10 MB).
const MaxArtifactBytes = 256 << 20

//go:embed release-signing.pub
var embeddedPublicKey string

// ReleasePublicKey is the public key release artifacts are signed with. It is
// compiled into the agent; agent.yaml can override it (the file is root-owned).
func ReleasePublicKey() (*PublicKey, error) { return ParsePublicKey(embeddedPublicKey) }

// TrustedComment is what the release workflow signs along with SHA256SUMS. It
// binds the signature to one version, so an old signed SHA256SUMS cannot be
// replayed as a newer release.
func TrustedComment(version string) string {
	return "use-my-mac release " + version + " file:" + SumsFile
}

// VerifySums checks the signature over SHA256SUMS and that it was made for
// version. It returns the parsed name -> sha256 map.
func VerifySums(pub *PublicKey, version string, sums, sig []byte) (map[string]string, error) {
	if len(sig) == 0 {
		return nil, errors.New("release is not signed (no " + SigFile + ")")
	}
	s, err := ParseSignature(sig)
	if err != nil {
		return nil, err
	}
	if err := Verify(pub, sums, s); err != nil {
		return nil, err
	}
	if s.TrustedComment != TrustedComment(version) {
		return nil, fmt.Errorf("signature is for %q, not %q", s.TrustedComment, TrustedComment(version))
	}
	return ParseSums(sums)
}

// ParseSums reads `sha256sum` output ("<hex>  <name>" or "<hex> *<name>").
func ParseSums(b []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 || len(f[0]) != 64 {
			return nil, fmt.Errorf("malformed %s line %q", SumsFile, line)
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			return nil, fmt.Errorf("malformed %s line %q", SumsFile, line)
		}
		out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
	}
	return out, sc.Err()
}

// CheckSum verifies data against the signed sums entry for name.
func CheckSum(sums map[string]string, name string, data []byte) error {
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s is not listed in %s", name, SumsFile)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s: sha256 mismatch", name)
	}
	return nil
}

// ExtractBinary returns the umm-agent binary from a verified bundle tarball.
func ExtractBinary(tgz []byte, bundle string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	want := bundle + "/umm-agent"
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in bundle", want)
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(strings.TrimPrefix(h.Name, "./")) != want {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s is not a regular file", want)
		}
		if h.Size <= 0 || h.Size > MaxArtifactBytes {
			return nil, fmt.Errorf("%s has an implausible size", want)
		}
		return io.ReadAll(io.LimitReader(tr, h.Size))
	}
}
