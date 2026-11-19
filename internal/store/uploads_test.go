package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestUploads runs on SQLite and, in CI, Postgres.
func TestUploads(t *testing.T) {
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
			old := &Upload{ID: "upl_" + rid(), Owner: "agent:a", Key: "jobs/j/t/out", BackendID: "b1",
				Size: 6 << 30, PartSize: 8 << 20, CreatedAt: 1, ExpiresAt: 100}
			fresh := &Upload{ID: "upl_" + rid(), Owner: "key:k", Key: "uploads/x", BackendID: "b2",
				Size: 10, PartSize: 5 << 20, Direct: true, CreatedAt: 1, ExpiresAt: 1 << 50}
			for _, u := range []*Upload{old, fresh} {
				if err := s.CreateUpload(ctx, u); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.GetUpload(ctx, fresh.ID)
			if err != nil || *got != *fresh {
				t.Fatalf("get: %v %+v", err, got)
			}
			exp, err := s.ExpiredUploads(ctx, 200, 1000)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, u := range exp {
				seen[u.ID] = true
			}
			if !seen[old.ID] || seen[fresh.ID] {
				t.Fatalf("expired: %v", seen)
			}
			for _, u := range []*Upload{old, fresh} {
				if err := s.DeleteUpload(ctx, u.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.GetUpload(ctx, old.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("after delete: %v", err)
			}
			if err := s.DeleteUpload(ctx, old.ID); err != nil {
				t.Fatalf("deleting twice: %v", err)
			}
		})
	}
}
