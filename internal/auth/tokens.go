// Package auth implements credentials (API keys, agent credentials,
// enrolment tokens), signed browser sessions and OIDC login with
// group-to-role mapping.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Prefixes make leaked secrets easy to recognise and scan for.
const (
	PrefixAPIKey = "umm_key_"
	PrefixAgent  = "umm_agt_"
	PrefixEnrol  = "umm_enr_"
	// PrefixInstall: one-line install tokens (enrolment secrets in an install URL).
	PrefixInstall = "umm_ins_"
)

// NewSecret returns prefix + 32 random bytes (base64url).
func NewSecret(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// NewID returns a short random identifier.
func NewID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Hash returns the SHA-256 of a secret; only hashes are stored.
func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Session is the payload of the signed session cookie.
type Session struct {
	Subject string `json:"sub"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Via     string `json:"via"`
	Expires int64  `json:"exp"`
}

// SignSession encodes and HMAC-signs a session.
func SignSession(key []byte, s Session) string {
	b, _ := json.Marshal(s)
	p := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// VerifySession checks the signature and expiry.
func VerifySession(key []byte, v string, now time.Time) (*Session, error) {
	p, sig, ok := strings.Cut(v, ".")
	if !ok {
		return nil, errors.New("malformed session")
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(p))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return nil, errors.New("bad session signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if now.Unix() > s.Expires {
		return nil, errors.New("session expired")
	}
	return &s, nil
}
