// Package selfupdate keeps umm-agent up to date. A small root updater
// (`umm-agent update`, run by its own LaunchDaemon) asks the router which
// version this Mac should run, downloads the release bundle (relayed by the
// router or straight from the release host), verifies a minisign signature
// over SHA256SUMS with a pinned public key, swaps the binary atomically,
// restarts the agent and rolls back if the new agent does not heartbeat.
//
// Everything here is pure Go and builds on every OS; only the launchctl
// service and the file lock are platform specific.
package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// minisign (https://jedisct1.github.io/minisign/) compatible ed25519 keys and
// signatures. Signatures made here verify with `minisign -V`, and the
// verifier accepts signatures made by minisign itself (prehashed "ED" and
// legacy "Ed").

var (
	algEd       = [2]byte{'E', 'd'} // key algorithm, and legacy (non-prehashed) signatures
	algEdPrehas = [2]byte{'E', 'D'} // BLAKE2b-512 prehashed signatures (minisign default)
)

// PublicKey is a minisign public key.
type PublicKey struct {
	KeyID [8]byte
	Key   ed25519.PublicKey
}

// SecretKey is an unencrypted minisign secret key (as made by `minisign -G -W`).
type SecretKey struct {
	KeyID [8]byte
	Key   ed25519.PrivateKey
}

// Signature is a parsed .minisig file.
type Signature struct {
	Algorithm       [2]byte
	KeyID           [8]byte
	Sig             []byte
	TrustedComment  string
	GlobalSignature []byte
}

// KeyIDString is the key ID as minisign prints it (upper-case hex, little-endian).
func KeyIDString(id [8]byte) string {
	return fmt.Sprintf("%016X", binary.LittleEndian.Uint64(id[:]))
}

// ParsePublicKey accepts the base64 key line on its own or a whole .pub file
// (an "untrusted comment:" line followed by the key line).
func ParsePublicKey(s string) (*PublicKey, error) {
	line := ""
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "untrusted comment:") {
			continue
		}
		line = l
		break
	}
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil || len(b) != 42 {
		return nil, errors.New("minisign: malformed public key")
	}
	if !bytes.Equal(b[:2], algEd[:]) {
		return nil, errors.New("minisign: unsupported public key algorithm")
	}
	pk := &PublicKey{Key: ed25519.PublicKey(append([]byte(nil), b[10:]...))}
	copy(pk.KeyID[:], b[2:10])
	return pk, nil
}

// String renders the public key as a minisign .pub file.
func (p *PublicKey) String() string {
	b := append(append(append([]byte{}, algEd[:]...), p.KeyID[:]...), p.Key...)
	return "untrusted comment: minisign public key " + KeyIDString(p.KeyID) + "\n" +
		base64.StdEncoding.EncodeToString(b) + "\n"
}

// ParseSignature parses a .minisig file.
func ParseSignature(data []byte) (*Signature, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "untrusted comment:") {
		return nil, errors.New("minisign: malformed signature (missing untrusted comment)")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(b) != 74 {
		return nil, errors.New("minisign: malformed signature line")
	}
	tc, ok := strings.CutPrefix(lines[2], "trusted comment: ")
	if !ok {
		return nil, errors.New("minisign: malformed signature (missing trusted comment)")
	}
	g, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || len(g) != ed25519.SignatureSize {
		return nil, errors.New("minisign: malformed global signature")
	}
	s := &Signature{Sig: b[10:], TrustedComment: tc, GlobalSignature: g}
	copy(s.Algorithm[:], b[:2])
	copy(s.KeyID[:], b[2:10])
	return s, nil
}

// Verify checks sig over msg with pub, including the signed trusted comment.
func Verify(pub *PublicKey, msg []byte, sig *Signature) error {
	if pub == nil || sig == nil {
		return errors.New("minisign: no key or signature")
	}
	if sig.KeyID != pub.KeyID {
		return fmt.Errorf("minisign: signed with key %s, expected %s", KeyIDString(sig.KeyID), KeyIDString(pub.KeyID))
	}
	var signed []byte
	switch sig.Algorithm {
	case algEdPrehas:
		h := blake2b.Sum512(msg)
		signed = h[:]
	case algEd:
		signed = msg
	default:
		return errors.New("minisign: unsupported signature algorithm")
	}
	if !ed25519.Verify(pub.Key, signed, sig.Sig) {
		return errors.New("minisign: signature verification failed")
	}
	global := append(append([]byte{}, sig.Sig...), []byte(sig.TrustedComment)...)
	if !ed25519.Verify(pub.Key, global, sig.GlobalSignature) {
		return errors.New("minisign: trusted comment signature verification failed")
	}
	return nil
}

// GenerateKey makes a new key pair.
func GenerateKey() (*PublicKey, *SecretKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, nil, err
	}
	return &PublicKey{KeyID: id, Key: pub}, &SecretKey{KeyID: id, Key: priv}, nil
}

// Public returns the matching public key.
func (k *SecretKey) Public() *PublicKey {
	return &PublicKey{KeyID: k.KeyID, Key: k.Key.Public().(ed25519.PublicKey)}
}

// Sign makes a prehashed ("ED") minisign signature with a trusted comment.
func (k *SecretKey) Sign(msg []byte, trustedComment string) []byte {
	if strings.ContainsAny(trustedComment, "\r\n") {
		panic("minisign: trusted comment must be one line")
	}
	h := blake2b.Sum512(msg)
	sig := ed25519.Sign(k.Key, h[:])
	global := ed25519.Sign(k.Key, append(append([]byte{}, sig...), []byte(trustedComment)...))
	line := append(append(append([]byte{}, algEdPrehas[:]...), k.KeyID[:]...), sig...)
	return []byte("untrusted comment: signature from use-my-mac release key\n" +
		base64.StdEncoding.EncodeToString(line) + "\n" +
		"trusted comment: " + trustedComment + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n")
}

// secret key file layout (unencrypted, kdf "\0\0"):
// sig_alg(2) kdf_alg(2) chk_alg(2) salt(32) opslimit(8) memlimit(8) key_id(8) sk(64) chk(32)
const secretKeyLen = 2 + 2 + 2 + 32 + 8 + 8 + 8 + 64 + 32

func (k *SecretKey) checksum() [32]byte {
	h, _ := blake2b.New256(nil)
	h.Write(algEd[:])
	h.Write(k.KeyID[:])
	h.Write(k.Key)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// String renders an unencrypted minisign secret key file.
func (k *SecretKey) String() string {
	b := make([]byte, 0, secretKeyLen)
	b = append(b, algEd[:]...)
	b = append(b, 0, 0)     // kdf: none
	b = append(b, 'B', '2') // checksum: BLAKE2b
	b = append(b, make([]byte, 32+8+8)...)
	b = append(b, k.KeyID[:]...)
	b = append(b, k.Key...)
	chk := k.checksum()
	b = append(b, chk[:]...)
	return "untrusted comment: minisign secret key (unencrypted)\n" + base64.StdEncoding.EncodeToString(b) + "\n"
}

// ParseSecretKey reads an unencrypted minisign secret key file. Encrypted
// keys are refused: CI has no password to give.
func ParseSecretKey(s string) (*SecretKey, error) {
	line := ""
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "untrusted comment:") {
			continue
		}
		line = l
		break
	}
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil || len(b) != secretKeyLen {
		return nil, errors.New("minisign: malformed secret key")
	}
	if !bytes.Equal(b[0:2], algEd[:]) || !bytes.Equal(b[4:6], []byte("B2")) {
		return nil, errors.New("minisign: unsupported secret key algorithm")
	}
	if b[2] != 0 || b[3] != 0 {
		return nil, errors.New("minisign: encrypted secret keys are not supported (generate with -W)")
	}
	off := 6 + 32 + 8 + 8
	k := &SecretKey{Key: ed25519.PrivateKey(append([]byte(nil), b[off+8:off+8+64]...))}
	copy(k.KeyID[:], b[off:off+8])
	chk := k.checksum()
	if !bytes.Equal(chk[:], b[off+8+64:]) {
		return nil, errors.New("minisign: secret key checksum mismatch")
	}
	return k, nil
}
