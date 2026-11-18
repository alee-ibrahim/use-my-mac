package store

import (
	"context"
	"os"
	"testing"
)

// TestAccessStore runs against SQLite always, and Postgres when
// UMM_TEST_POSTGRES_URL is set.
func TestAccessStore(t *testing.T) {
	urls := map[string]string{"sqlite": "sqlite://" + t.TempDir() + "/a.db"}
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
			v := "grp-" + rid()
			r, err := s.PutAccessRule(ctx, AccessRule{ID: rid(), Kind: "group", Value: v, Role: "viewer", CreatedBy: "key:a", CreatedAt: 10})
			if err != nil || r.Role != "viewer" || r.UpdatedAt != 0 {
				t.Fatalf("put: %+v %v", r, err)
			}
			// Same kind+value updates the role and keeps the ID.
			r2, err := s.PutAccessRule(ctx, AccessRule{ID: rid(), Kind: "group", Value: v, Role: "admin", CreatedAt: 20})
			if err != nil || r2.ID != r.ID || r2.Role != "admin" || r2.UpdatedAt != 20 || r2.CreatedBy != "key:a" {
				t.Fatalf("update: %+v %v", r2, err)
			}
			if err := s.DeleteAccessRule(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteAccessRule(ctx, r.ID); err != ErrNotFound {
				t.Fatalf("delete twice: %v", err)
			}

			sub := "sub-" + rid()
			u := AccessUser{Sub: sub, Name: "A", Groups: []string{"g1", "g2"}, Overage: true, FirstLogin: 5, LastLogin: 5}
			if err := s.UpsertAccessUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			u.Name, u.Groups, u.FirstLogin, u.LastLogin, u.Overage = "B", nil, 9, 9, false
			if err := s.UpsertAccessUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			us, err := s.AccessUsers(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var got *AccessUser
			for i := range us {
				if us[i].Sub == sub {
					got = &us[i]
				}
			}
			if got == nil || got.Name != "B" || got.FirstLogin != 5 || got.LastLogin != 9 || len(got.Groups) != 0 || got.Overage {
				t.Fatalf("user: %+v", got)
			}

			if err := s.SetAccessGroupName(ctx, v, "Ops"); err != nil {
				t.Fatal(err)
			}
			s.SetAccessGroupName(ctx, v, "Ops 2")
			names, _ := s.AccessGroupNames(ctx)
			if names[v] != "Ops 2" {
				t.Fatalf("names: %v", names)
			}
			s.SetAccessGroupName(ctx, v, "")
			if names, _ = s.AccessGroupNames(ctx); names[v] != "" {
				t.Fatal("name not removed")
			}

			if err := s.AddAccessEvent(ctx, AccessEvent{TS: 1 << 50, Actor: "x", Action: "rule.set", Detail: v}); err != nil {
				t.Fatal(err)
			}
			evs, err := s.AccessEvents(ctx, 1)
			if err != nil || len(evs) != 1 || evs[0].Detail != v {
				t.Fatalf("events: %+v %v", evs, err)
			}
		})
	}
}
