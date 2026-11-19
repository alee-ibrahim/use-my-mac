package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestMigrateOldDatabase opens a database created by a version without the
// added columns and tables (agents and jobs without admin labels,
// reservations, router max_tasks or cleanup; no blobs table), with rows in
// it, and checks Open upgrades it in place, twice. SQLite always; Postgres
// in its own schema when UMM_TEST_POSTGRES_URL is set.
func TestMigrateOldDatabase(t *testing.T) {
	ctx := context.Background()
	type target struct{ driver, dsn, url string }
	var targets []target
	path := t.TempDir() + "/old.db"
	targets = append(targets, target{"sqlite", "file:" + path, "sqlite://" + path})
	if pg := os.Getenv("UMM_TEST_POSTGRES_URL"); pg != "" {
		schemaName := "mig_" + rid()
		db, err := sql.Open("pgx", pg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schemaName); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			db.ExecContext(ctx, `DROP SCHEMA `+schemaName+` CASCADE`)
			db.Close()
		})
		u, err := url.Parse(pg)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schemaName)
		u.RawQuery = q.Encode()
		targets = append(targets, target{"pgx", u.String(), u.String()})
	}
	for _, tg := range targets {
		t.Run(tg.driver, func(t *testing.T) {
			old, err := sql.Open(tg.driver, tg.dsn)
			if err != nil {
				t.Fatal(err)
			}
			// The old schema: every table as first released, no added
			// columns, no blobs table.
			for _, q := range schema {
				if strings.Contains(q, " blobs") {
					continue
				}
				if _, err := old.ExecContext(ctx, q); err != nil {
					t.Fatalf("old schema: %v\n%s", err, q)
				}
			}
			ins := func(q string, args ...any) {
				t.Helper()
				if tg.driver == "pgx" {
					s := &Store{postgres: true}
					q = s.rebind(q)
				}
				if _, err := old.ExecContext(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			ins(`INSERT INTO agents(id, name, cred_hash, created_at, labels, max_tasks) VALUES(?,?,?,?,?,?)`,
				"mac_old", "Old Mac", "h_"+rid(), 1, `{"room":"2"}`, 2)
			ins(`INSERT INTO jobs(id, name, owner, labels, state, created_at, finished_at) VALUES(?,?,?,?,?,?,?)`,
				"job_old", "old", "key:k", "{}", "succeeded", 1, 2)
			old.Close()

			for i := range 2 {
				s, err := Open(ctx, tg.url)
				if err != nil {
					t.Fatalf("open %d: %v", i, err)
				}
				a, err := s.GetAgent(ctx, "mac_old")
				if err != nil {
					t.Fatal(err)
				}
				if a.AdminLabels != "{}" || a.ReservedFor != "[]" || a.ReservedUntil != 0 || a.MaxTasksSet != 0 || a.Labels != `{"room":"2"}` {
					t.Fatalf("agent after migration: %+v", a)
				}
				if i == 0 {
					if err := s.SetAgentReservation(ctx, "mac_old", `["key:k"]`, 5, "n", "admin"); err != nil {
						t.Fatal(err)
					}
					if err := s.SetAgentReservation(ctx, "mac_old", `[]`, 0, "", ""); err != nil {
						t.Fatal(err)
					}
				}
				j, err := s.GetJob(ctx, "job_old")
				if err != nil || j.Cleanup != "" || j.CleanedAt != 0 || j.State != "succeeded" {
					t.Fatalf("job after migration: %+v %v", j, err)
				}
				if err := s.PutBlob(ctx, &Blob{Key: "uploads/x" + rid(), Owner: "key:k", Kind: BlobUpload, CreatedAt: 1}); err != nil {
					t.Fatal(err)
				}
				s.Close()
			}
		})
	}
}

// TestBlobsAndNewColumns covers the blob ownership table and the added
// agent/job columns on SQLite and, in CI, Postgres.
func TestBlobsAndNewColumns(t *testing.T) {
	urls := map[string]string{"sqlite": "sqlite://" + t.TempDir() + "/t.db"}
	if pg := os.Getenv("UMM_TEST_POSTGRES_URL"); pg != "" {
		urls["postgres"] = pg
	}
	for name, u := range urls {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, u)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			job := "job_" + rid()
			owner := "key:" + rid()
			up := &Blob{Key: "uploads/" + rid() + "/in.mov", Owner: owner, Kind: BlobUpload, Size: 10, CreatedAt: 1}
			out := &Blob{Key: "jobs/" + job + "/t/out", Owner: owner, Kind: BlobOutput, JobID: job, TaskID: "tsk", Size: 20, CreatedAt: 2}
			for _, b := range []*Blob{up, out} {
				if err := s.PutBlob(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.MarkBlobDownloaded(ctx, out.Key, 7); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.GetBlob(ctx, out.Key); got.DownloadedAt != 7 {
				t.Fatalf("downloaded: %+v", got)
			}
			// Re-writing a key resets the download mark.
			if err := s.PutBlob(ctx, out); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.GetBlob(ctx, out.Key); got.DownloadedAt != 0 || got.Size != 20 {
				t.Fatalf("rewritten: %+v", got)
			}
			all, err := s.ListBlobs(ctx, BlobFilter{Owner: owner})
			if err != nil || len(all) != 2 || all[0].Key != out.Key {
				t.Fatalf("list: %+v %v", all, err)
			}
			if bs, _ := s.ListBlobs(ctx, BlobFilter{Owner: owner, Prefix: "uploads/"}); len(bs) != 1 || bs[0].Key != up.Key {
				t.Fatalf("prefix: %+v", bs)
			}
			if bs, _ := s.ListBlobs(ctx, BlobFilter{JobID: job, Kind: BlobOutput}); len(bs) != 1 {
				t.Fatalf("by job: %+v", bs)
			}
			if err := s.DeleteJobBlobRows(ctx, job); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetBlob(ctx, out.Key); !errors.Is(err, ErrNotFound) {
				t.Fatal("job rows must go")
			}
			if err := s.DeleteBlobRow(ctx, up.Key); err != nil {
				t.Fatal(err)
			}

			a := &Agent{ID: "a" + rid(), Name: "m", CredHash: "c" + rid(), CreatedAt: 1, Runtimes: "[]", Labels: "{}", MaxTasks: 1}
			if err := s.CreateAgent(ctx, a); err != nil {
				t.Fatal(err)
			}
			if err := s.SetAgentAdminLabels(ctx, a.ID, `{"class":"studio"}`); err != nil {
				t.Fatal(err)
			}
			if err := s.SetAgentMaxTasks(ctx, a.ID, 4); err != nil {
				t.Fatal(err)
			}
			if err := s.SetAgentReservation(ctx, a.ID, `["key:k"]`, 99, "note", "admin"); err != nil {
				t.Fatal(err)
			}
			got, _ := s.GetAgent(ctx, a.ID)
			if got.AdminLabels != `{"class":"studio"}` || got.MaxTasksSet != 4 || got.ReservedFor != `["key:k"]` ||
				got.ReservedUntil != 99 || got.ReservedNote != "note" || got.ReservedBy != "admin" {
				t.Fatalf("agent: %+v", got)
			}
			if err := s.SetAgentMaxTasks(ctx, "nope", 1); !errors.Is(err, ErrNotFound) {
				t.Fatal("unknown agent")
			}

			j := &Job{ID: job, Name: "j", Owner: owner, Labels: "{}", State: "succeeded", CreatedAt: 1, FinishedAt: 5,
				Cleanup: `{"after_hours":1}`}
			if err := s.CreateJob(ctx, j, nil); err != nil {
				t.Fatal(err)
			}
			due, err := s.JobsDueCleanup(ctx, 1000)
			found := false
			for _, d := range due {
				found = found || d.ID == job
			}
			if err != nil || !found {
				t.Fatalf("due: %v %v", due, err)
			}
			if err := s.SetJobCleaned(ctx, job, 9); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.GetJob(ctx, job); got.CleanedAt != 9 || got.Cleanup != `{"after_hours":1}` {
				t.Fatalf("job: %+v", got)
			}
			ids, err := s.FinishedJobsOf(ctx, owner, 100, nil, 10)
			if err != nil || len(ids) != 1 || ids[0] != job {
				t.Fatalf("finished of owner: %v %v", ids, err)
			}
			if ids, _ := s.FinishedJobsOf(ctx, "key:other", 100, nil, 10); len(ids) != 0 {
				t.Fatalf("other owner: %v", ids)
			}
		})
	}
}
