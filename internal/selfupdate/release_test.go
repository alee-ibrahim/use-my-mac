package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// testRelease builds the artifacts of a release: bundle tarball, SHA256SUMS
// and (when sk != nil) its signature.
func testRelease(t testing.TB, sk *SecretKey, version, goos, goarch string, bin []byte) map[string][]byte {
	t.Helper()
	bundle := BundleName(goos, goarch)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct {
		name string
		data []byte
	}{{bundle + "/install.sh", []byte("#!/bin/sh\n")}, {bundle + "/umm-agent", bin}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.data)
	}
	tw.Close()
	zw.Close()
	tgz := buf.Bytes()
	sum := sha256.Sum256(tgz)
	other := sha256.Sum256([]byte("other"))
	sums := fmt.Sprintf("%s  %s\n%s  umm-linux-amd64\n", hex.EncodeToString(sum[:]), BundleFile(goos, goarch), hex.EncodeToString(other[:]))
	out := map[string][]byte{SumsFile: []byte(sums), BundleFile(goos, goarch): tgz}
	if sk != nil {
		out[SigFile] = sk.Sign([]byte(sums), TrustedComment(version))
	}
	return out
}

func TestVerifyRelease(t *testing.T) {
	pub, sk, _ := GenerateKey()
	_, otherSK, _ := GenerateKey()
	bin := []byte("BIN:v1.2.3")
	rel := testRelease(t, sk, "v1.2.3", "darwin", "arm64", bin)
	name := BundleFile("darwin", "arm64")

	sums, err := VerifySums(pub, "v1.2.3", rel[SumsFile], rel[SigFile])
	if err != nil {
		t.Fatalf("valid release rejected: %v", err)
	}
	if err := CheckSum(sums, name, rel[name]); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractBinary(rel[name], BundleName("darwin", "arm64"))
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("extract: %v %q", err, got)
	}

	// Unsigned.
	if _, err := VerifySums(pub, "v1.2.3", rel[SumsFile], nil); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("unsigned accepted: %v", err)
	}
	// Signed by someone else.
	if _, err := VerifySums(pub, "v1.2.3", rel[SumsFile], otherSK.Sign(rel[SumsFile], TrustedComment("v1.2.3"))); err == nil {
		t.Fatal("foreign signature accepted")
	}
	// SHA256SUMS edited after signing.
	tampered := bytes.Replace(rel[SumsFile], []byte("umm-linux-amd64"), []byte("umm-linux-arm64"), 1)
	if _, err := VerifySums(pub, "v1.2.3", tampered, rel[SigFile]); err == nil {
		t.Fatal("tampered SHA256SUMS accepted")
	}
	// An old release's signed sums replayed as a newer version.
	if _, err := VerifySums(pub, "v9.9.9", rel[SumsFile], rel[SigFile]); err == nil || !strings.Contains(err.Error(), "not") {
		t.Fatalf("replayed signature accepted: %v", err)
	}
	// Tarball swapped (checksum mismatch).
	evil := testRelease(t, sk, "v1.2.3", "darwin", "arm64", []byte("EVIL"))[name]
	if err := CheckSum(sums, name, evil); err == nil {
		t.Fatal("tampered tarball accepted")
	}
	if err := CheckSum(sums, "missing.tar.gz", evil); err == nil {
		t.Fatal("unlisted file accepted")
	}
	if _, err := ParseSums([]byte("nothex  file\n")); err == nil {
		t.Fatal("malformed sums parsed")
	}
	if _, err := ExtractBinary(rel[name], "umm-agent-linux-amd64"); err == nil {
		t.Fatal("wrong bundle extracted")
	}
}
