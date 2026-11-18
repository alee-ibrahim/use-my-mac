package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func testProfile() api.InstallProfile {
	return api.InstallProfile{Name: "default", Labels: map[string]string{"site": "a"},
		Runtimes: map[string][]string{"sh": {"/bin/sh"}}}
}

func (h *harness) enrolWith(token, name string) (api.EnrolResponse, error) {
	return h.e.EnrolFrom(h.ctx, api.EnrolRequest{Token: token, Name: name, Runtimes: []string{"sh"}, MaxTasks: 1}, "192.0.2.7")
}

func TestInstallTokenLifecycle(t *testing.T) {
	h := newHarness(t)
	tok, err := h.e.CreateInstallToken(h.ctx, "key:admin", testProfile(), api.InstallTokenRequest{
		Labels: map[string]string{"room": "lab-2", "site": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	// Defaults: one use, one hour; extra labels override the profile's.
	if tok.MaxUses != 1 || !tok.ExpiresAt.Equal(h.now.Add(time.Hour).UTC()) || tok.State != api.InstallActive {
		t.Fatalf("defaults: %+v", tok)
	}
	if tok.Profile.Labels["site"] != "b" || tok.Profile.Labels["room"] != "lab-2" || tok.Profile.ServiceLabel == "" {
		t.Fatalf("profile not bound/merged: %+v", tok.Profile)
	}
	if !strings.HasPrefix(tok.Token, auth.PrefixInstall) || !strings.HasPrefix(tok.Token, tok.Prefix) {
		t.Fatalf("token %q prefix %q", tok.Token, tok.Prefix)
	}

	// Hashed at rest: the secret is nowhere in the row.
	row, err := h.e.st.GetInstallToken(h.ctx, tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Hash != auth.Hash(tok.Token) || strings.Contains(row.Hash+row.Spec+row.Prefix+row.Note, tok.Token) {
		t.Fatal("token is not stored hashed")
	}

	// Looking it up (script fetch) does not use it up.
	for range 3 {
		if _, err := h.e.LookupInstallToken(h.ctx, tok.Token); err != nil {
			t.Fatal(err)
		}
	}
	h.advance(time.Second)
	if _, err := h.enrolWith(tok.Token, "mac-1"); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)
	if _, err := h.enrolWith(tok.Token, "mac-2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("second use of a single-use token: %v", err)
	}
	if v, err := h.e.LookupInstallToken(h.ctx, tok.Token); !errors.Is(err, ErrInstallToken) || v.State != api.InstallUsed {
		t.Fatalf("used token lookup: %v %s", err, v.State)
	}
	evs, _ := h.e.InstallEvents(h.ctx, tok.ID)
	kinds := []string{}
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "denied,enrol,create" {
		t.Fatalf("audit log %s", got)
	}
	if evs[1].IP != "192.0.2.7" || evs[1].AgentID == "" {
		t.Fatalf("enrol event %+v", evs[1])
	}

	// Unknown and malformed tokens.
	for _, bad := range []string{"", "umm_ins_nope", auth.NewSecret(auth.PrefixInstall), "umm_ins_' OR 1=1 --"} {
		if _, err := h.e.LookupInstallToken(h.ctx, bad); !errors.Is(err, ErrInstallToken) {
			t.Errorf("lookup %q: %v", bad, err)
		}
		if _, err := h.enrolWith(bad, "x"); !errors.Is(err, ErrForbidden) {
			t.Errorf("enrol %q: %v", bad, err)
		}
	}
}

func TestInstallTokenUsesExpiryRevoke(t *testing.T) {
	h := newHarness(t)
	multi, err := h.e.CreateInstallToken(h.ctx, "u", testProfile(), api.InstallTokenRequest{MaxUses: 3, TTLMinutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := h.enrolWith(multi.Token, "batch-"+string(rune('a'+i))); err != nil {
			t.Fatalf("use %d: %v", i+1, err)
		}
	}
	if _, err := h.enrolWith(multi.Token, "batch-d"); !errors.Is(err, ErrForbidden) {
		t.Fatal("fourth use of a 3-use token accepted")
	}

	exp, _ := h.e.CreateInstallToken(h.ctx, "u", testProfile(), api.InstallTokenRequest{TTLMinutes: 10})
	h.advance(10*time.Minute + time.Millisecond)
	if v, err := h.e.LookupInstallToken(h.ctx, exp.Token); !errors.Is(err, ErrInstallToken) || v.State != api.InstallExpired {
		t.Fatalf("expired: %v %s", err, v.State)
	}
	if _, err := h.enrolWith(exp.Token, "late"); !errors.Is(err, ErrForbidden) {
		t.Fatal("expired token accepted")
	}

	rev, _ := h.e.CreateInstallToken(h.ctx, "u", testProfile(), api.InstallTokenRequest{MaxUses: 5})
	if err := h.e.RevokeInstallToken(h.ctx, rev.ID, "u", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.e.RevokeInstallToken(h.ctx, rev.ID, "u", ""); err != nil {
		t.Fatal("revoke is not idempotent:", err)
	}
	if err := h.e.RevokeInstallToken(h.ctx, "ins_missing", "u", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown: %v", err)
	}
	if _, err := h.enrolWith(rev.Token, "x"); !errors.Is(err, ErrForbidden) {
		t.Fatal("revoked token accepted")
	}
	list, _ := h.e.InstallTokens(h.ctx)
	states := map[string]string{}
	for _, v := range list {
		states[v.ID] = v.State
		if v.Token != "" {
			t.Fatal("list leaks the secret")
		}
	}
	if states[multi.ID] != api.InstallUsed || states[exp.ID] != api.InstallExpired || states[rev.ID] != api.InstallRevoked {
		t.Fatalf("states %v", states)
	}

	// Bounds.
	for _, req := range []api.InstallTokenRequest{{TTLMinutes: -1}, {TTLMinutes: 7*24*60 + 1}, {MaxUses: 1001}, {MaxUses: -2},
		{Labels: map[string]string{"x": "a b"}}, {Note: "two\nlines"}} {
		if _, err := h.e.CreateInstallToken(h.ctx, "u", testProfile(), req); err == nil {
			t.Errorf("accepted %+v", req)
		}
	}
}
