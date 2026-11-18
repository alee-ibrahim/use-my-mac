package auth

import (
	"strings"
	"testing"
	"time"
)

func TestSecretsAndHash(t *testing.T) {
	a, b := NewSecret(PrefixAPIKey), NewSecret(PrefixAPIKey)
	if a == b || !strings.HasPrefix(a, PrefixAPIKey) || len(a) < 40 {
		t.Fatalf("bad secrets %q %q", a, b)
	}
	if Hash(a) == Hash(b) || Hash(a) != Hash(a) || len(Hash(a)) != 64 {
		t.Fatal("hash")
	}
}

func TestSessionSignVerify(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1000, 0)
	v := SignSession(key, Session{Subject: "u1", Role: "viewer", Expires: 2000})
	s, err := VerifySession(key, v, now)
	if err != nil || s.Role != "viewer" {
		t.Fatalf("%v %+v", err, s)
	}
	if _, err := VerifySession([]byte("another-key-another-key-another!!"), v, now); err == nil {
		t.Fatal("wrong key must fail")
	}
	// Tamper with the payload (escalate role) keeping the old signature.
	_, sig, _ := strings.Cut(v, ".")
	forged := SignSession([]byte("x"), Session{Subject: "u1", Role: "admin", Expires: 2000})
	fp, _, _ := strings.Cut(forged, ".")
	if _, err := VerifySession(key, fp+"."+sig, now); err == nil {
		t.Fatal("tampered payload must fail")
	}
	if _, err := VerifySession(key, v, time.Unix(3000, 0)); err == nil {
		t.Fatal("expired must fail")
	}
}

func TestLoginStateRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	st := LoginState{State: "s", Nonce: "n", Verifier: "v", Return: "/", Expires: 2000}
	enc := EncodeState(key, st)
	got, err := DecodeState(key, enc, time.Unix(1000, 0))
	if err != nil || *got != st {
		t.Fatalf("%v %+v", err, got)
	}
	// A normal session cookie must not be accepted as login state.
	sess := SignSession(key, Session{Subject: "x", Expires: 2000})
	if _, err := DecodeState(key, sess, time.Unix(1000, 0)); err == nil {
		t.Fatal("session accepted as login state")
	}
}

func TestEntraPreset(t *testing.T) {
	c := OIDCConfig{Preset: "entra", TenantID: "00000000-0000-0000-0000-000000000000", ClientID: "app"}
	c.ApplyPreset()
	if c.Issuer != "https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000/v2.0" {
		t.Fatalf("issuer %q", c.Issuer)
	}
	if c.GroupsClaim != "groups" || c.RolesClaim != "roles" || !c.Enabled() {
		t.Fatalf("%+v", c)
	}
}

func TestMapRole(t *testing.T) {
	c := OIDCConfig{
		GroupsClaim:  "groups",
		RolesClaim:   "roles",
		AdminGroups:  []string{"11111111-aaaa", "umm.admin"},
		ViewerGroups: []string{"22222222-bbbb"},
	}
	cases := []struct {
		name   string
		claims map[string]any
		role   string
		err    error
	}{
		{"admin group", map[string]any{"groups": []any{"x", "11111111-aaaa"}}, "admin", nil},
		{"admin wins", map[string]any{"groups": []any{"22222222-bbbb", "11111111-AAAA"}}, "admin", nil},
		{"viewer group", map[string]any{"groups": []any{"22222222-bbbb"}}, "viewer", nil},
		{"app role", map[string]any{"roles": []any{"umm.admin"}}, "admin", nil},
		{"string claim", map[string]any{"groups": "a 22222222-bbbb"}, "viewer", nil},
		{"no group", map[string]any{"groups": []any{"zzz"}}, "", ErrNoRole},
		{"missing claim", map[string]any{}, "", ErrNoRole},
		{"overage", map[string]any{"_claim_names": map[string]any{"groups": "src1"}}, "", ErrGroupOverage},
	}
	for _, tc := range cases {
		role, err := c.MapRole(tc.claims)
		if role != tc.role || err != tc.err {
			t.Errorf("%s: got %q %v want %q %v", tc.name, role, err, tc.role, tc.err)
		}
	}
	c.DefaultRole = "viewer"
	if role, err := c.MapRole(map[string]any{"groups": []any{"zzz"}}); role != "viewer" || err != nil {
		t.Errorf("default role: %q %v", role, err)
	}
}
