package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

func TestMinisignRoundTrip(t *testing.T) {
	pub, sk, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello release\n")
	sig, err := ParseSignature(sk.Sign(msg, "use-my-mac release v1.2.3 file:SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, msg, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if sig.TrustedComment != "use-my-mac release v1.2.3 file:SHA256SUMS" {
		t.Fatalf("trusted comment %q", sig.TrustedComment)
	}

	// Key files round-trip.
	pub2, err := ParsePublicKey(pub.String())
	if err != nil || pub2.KeyID != pub.KeyID || !pub2.Key.Equal(pub.Key) {
		t.Fatalf("public key round trip: %v", err)
	}
	sk2, err := ParseSecretKey(sk.String())
	if err != nil || sk2.KeyID != sk.KeyID || !sk2.Key.Equal(sk.Key) {
		t.Fatalf("secret key round trip: %v", err)
	}
	// The bare key line works too (agent.yaml public_key).
	line := strings.Split(strings.TrimSpace(pub.String()), "\n")[1]
	if _, err := ParsePublicKey(line); err != nil {
		t.Fatal(err)
	}
}

func TestMinisignRejects(t *testing.T) {
	pub, sk, _ := GenerateKey()
	other, _, _ := GenerateKey()
	msg := []byte("payload")
	raw := sk.Sign(msg, "tc")
	sig, _ := ParseSignature(raw)

	if Verify(pub, []byte("payloaD"), sig) == nil {
		t.Fatal("tampered message accepted")
	}
	wrongKey := *other
	if Verify(&wrongKey, msg, sig) == nil {
		t.Fatal("wrong key ID accepted")
	}
	// Same key ID, different key material.
	forged := &PublicKey{KeyID: pub.KeyID, Key: other.Key}
	if Verify(forged, msg, sig) == nil {
		t.Fatal("wrong key accepted")
	}
	// Changing the trusted comment breaks the global signature.
	tampered := strings.Replace(string(raw), "trusted comment: tc", "trusted comment: tc2", 1)
	sig2, err := ParseSignature([]byte(tampered))
	if err != nil {
		t.Fatal(err)
	}
	if Verify(pub, msg, sig2) == nil {
		t.Fatal("tampered trusted comment accepted")
	}
	for _, bad := range []string{"", "garbage", "untrusted comment: x\nAAAA\ntrusted comment: y\nAAAA\n"} {
		if _, err := ParseSignature([]byte(bad)); err == nil {
			t.Fatalf("malformed signature %q parsed", bad)
		}
	}
	// A corrupted secret key fails its checksum.
	s := sk.String()
	b, _ := base64.StdEncoding.DecodeString(strings.Split(strings.TrimSpace(s), "\n")[1])
	b[len(b)-40] ^= 1
	if _, err := ParseSecretKey(base64.StdEncoding.EncodeToString(b)); err == nil {
		t.Fatal("corrupted secret key accepted")
	}
}

// Legacy (non-prehashed "Ed") signatures from older minisign still verify.
func TestMinisignLegacy(t *testing.T) {
	pub, sk, _ := GenerateKey()
	msg := []byte("legacy")
	s := ed25519.Sign(sk.Key, msg)
	g := ed25519.Sign(sk.Key, append(append([]byte{}, s...), "tc"...))
	line := append(append([]byte("Ed"), sk.KeyID[:]...), s...)
	raw := "untrusted comment: x\n" + base64.StdEncoding.EncodeToString(line) + "\ntrusted comment: tc\n" +
		base64.StdEncoding.EncodeToString(g) + "\n"
	sig, err := ParseSignature([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, msg, sig); err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddedPublicKey(t *testing.T) {
	pub, err := ReleasePublicKey()
	if err != nil {
		t.Fatalf("embedded release key: %v", err)
	}
	if KeyIDString(pub.KeyID) == "" {
		t.Fatal("no key id")
	}
}
