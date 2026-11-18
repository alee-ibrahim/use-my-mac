package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestInstallTokens runs on SQLite and, in CI, Postgres.
func TestInstallTokens(t *testing.T) {
	urls := map[string]string{"sqlite": "sqlite://" + t.TempDir() + "/t.db"}
	if pg := os.Getenv("UMM_TEST_POSTGRES_URL"); pg != "" {
		urls["postgres"] = pg
	}
	for name, url := range urls {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id, hash := "ins_"+rid(), "h"+rid()
			if err := s.CreateInstallToken(ctx, &InstallToken{ID: id, Hash: hash, Prefix: "p", Profile: "default", Spec: "{}",
				CreatedAt: 1, ExpiresAt: 1000, MaxUses: 2}); err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				tok, err := s.ConsumeInstallToken(ctx, hash, 10)
				if err != nil || tok.Uses != i+1 || tok.LastUsedAt != 10 {
					t.Fatalf("use %d: %v %+v", i+1, err, tok)
				}
			}
			if _, err := s.ConsumeInstallToken(ctx, hash, 10); !errors.Is(err, ErrNotFound) {
				t.Fatalf("third use: %v", err)
			}
			id2, hash2 := "ins_"+rid(), "h"+rid()
			s.CreateInstallToken(ctx, &InstallToken{ID: id2, Hash: hash2, Prefix: "p", Profile: "default", Spec: "{}",
				CreatedAt: 2, ExpiresAt: 1000, MaxUses: 5})
			if _, err := s.ConsumeInstallToken(ctx, hash2, 1000); !errors.Is(err, ErrNotFound) {
				t.Fatal("expired token consumed")
			}
			if err := s.RevokeInstallToken(ctx, id2, 20); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ConsumeInstallToken(ctx, hash2, 30); !errors.Is(err, ErrNotFound) {
				t.Fatal("revoked token consumed")
			}
			if err := s.RevokeInstallToken(ctx, "ins_nope"+rid(), 20); !errors.Is(err, ErrNotFound) {
				t.Fatalf("revoke unknown: %v", err)
			}
			for i := range 5 {
				if err := s.AddInstallEvent(ctx, &InstallEvent{TokenID: id, TS: int64(100 + i), Kind: "script", IP: "192.0.2.1"}, 3); err != nil {
					t.Fatal(err)
				}
			}
			evs, err := s.InstallEvents(ctx, id, 0)
			if err != nil || len(evs) != 3 || evs[0].TS != 104 {
				t.Fatalf("events: %v %+v", err, evs)
			}
			list, err := s.ListInstallTokens(ctx, 0)
			if err != nil || len(list) < 2 {
				t.Fatalf("list: %v %d", err, len(list))
			}
		})
	}
}
