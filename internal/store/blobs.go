package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// Blob ownership. Every blob the router stores for an owner (an app upload
// or a task output) has a row here, so owners can list and delete their own
// files and nobody else can read or overwrite them. The bytes live in the
// storage backend. Blobs stored by versions before ownership was tracked
// have no row.

func init() {
	schema = append(schema,
		`CREATE TABLE IF NOT EXISTS blobs (
			key TEXT PRIMARY KEY,
			owner TEXT NOT NULL,
			kind TEXT NOT NULL,
			job_id TEXT NOT NULL DEFAULT '',
			task_id TEXT NOT NULL DEFAULT '',
			size BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			downloaded_at BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS blobs_owner ON blobs(owner)`,
		`CREATE INDEX IF NOT EXISTS blobs_job ON blobs(job_id)`,
	)
}

// Blob kinds.
const (
	BlobUpload = "upload"
	BlobOutput = "output"
)

// Blob is a row of blobs. Key is the full storage key (uploads/<key> or
// jobs/<job>/<task>/<output>).
type Blob struct {
	Key, Owner, Kind, JobID, TaskID string
	Size, CreatedAt, DownloadedAt   int64
}

const blobCols = `key, owner, kind, job_id, task_id, size, created_at, downloaded_at`

func scanBlob(sc interface{ Scan(...any) error }) (*Blob, error) {
	var b Blob
	err := sc.Scan(&b.Key, &b.Owner, &b.Kind, &b.JobID, &b.TaskID, &b.Size, &b.CreatedAt, &b.DownloadedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &b, err
}

// PutBlob records a blob (insert, or replace the row of a key that was
// written again: owner, size and time are updated, the download mark is
// cleared).
func (s *Store) PutBlob(ctx context.Context, b *Blob) error {
	_, err := s.exec(ctx, `INSERT INTO blobs(`+blobCols+`) VALUES(?,?,?,?,?,?,?,0)
		ON CONFLICT (key) DO UPDATE SET owner = excluded.owner, kind = excluded.kind, job_id = excluded.job_id,
		task_id = excluded.task_id, size = excluded.size, created_at = excluded.created_at, downloaded_at = 0`,
		b.Key, b.Owner, b.Kind, b.JobID, b.TaskID, b.Size, b.CreatedAt)
	return err
}

// GetBlob returns a blob's row (ErrNotFound when untracked).
func (s *Store) GetBlob(ctx context.Context, key string) (*Blob, error) {
	return scanBlob(s.queryRow(ctx, `SELECT `+blobCols+` FROM blobs WHERE key = ?`, key))
}

// DeleteBlobRow forgets a blob (missing rows are not an error).
func (s *Store) DeleteBlobRow(ctx context.Context, key string) error {
	_, err := s.exec(ctx, `DELETE FROM blobs WHERE key = ?`, key)
	return err
}

// MarkBlobDownloaded records that a blob was downloaded in full.
func (s *Store) MarkBlobDownloaded(ctx context.Context, key string, at int64) error {
	_, err := s.exec(ctx, `UPDATE blobs SET downloaded_at = ? WHERE key = ? AND downloaded_at = 0`, at, key)
	return err
}

// BlobFilter selects blobs. Empty fields match everything.
type BlobFilter struct {
	Owner, Kind, JobID, Prefix string
	Limit                      int
}

// ListBlobs returns blobs, newest first.
func (s *Store) ListBlobs(ctx context.Context, f BlobFilter) ([]*Blob, error) {
	if f.Limit <= 0 || f.Limit > 10000 {
		f.Limit = 1000
	}
	q := `SELECT ` + blobCols + ` FROM blobs WHERE 1=1`
	var args []any
	if f.Owner != "" {
		q += ` AND owner = ?`
		args = append(args, f.Owner)
	}
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.JobID != "" {
		q += ` AND job_id = ?`
		args = append(args, f.JobID)
	}
	if f.Prefix != "" {
		// Prefix match without LIKE wildcards: keys are [A-Za-z0-9._-/].
		q += ` AND SUBSTR(key, 1, ?) = ?`
		args = append(args, len(f.Prefix), f.Prefix)
	}
	q += ` ORDER BY created_at DESC, key LIMIT ` + strconv.Itoa(f.Limit)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Blob
	for rows.Next() {
		b, err := scanBlob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteJobBlobRows forgets every output row of a job.
func (s *Store) DeleteJobBlobRows(ctx context.Context, jobID string) error {
	_, err := s.exec(ctx, `DELETE FROM blobs WHERE job_id = ? AND kind = ?`, jobID, BlobOutput)
	return err
}

// JobsDueCleanup returns finished jobs with a cleanup policy that has not
// run yet, oldest first.
func (s *Store) JobsDueCleanup(ctx context.Context, limit int) ([]*Job, error) {
	rows, err := s.query(ctx, `SELECT `+jobCols+` FROM jobs WHERE cleanup <> '' AND cleaned_at = 0 AND finished_at <> 0
		ORDER BY finished_at LIMIT `+strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetJobCleaned records that a job's cleanup policy ran.
func (s *Store) SetJobCleaned(ctx context.Context, id string, at int64) error {
	_, err := s.exec(ctx, `UPDATE jobs SET cleaned_at = ? WHERE id = ?`, at, id)
	return err
}

// UploadsUnder returns unfinished multipart uploads whose key starts with
// prefix (e.g. a deleted job's outputs).
func (s *Store) UploadsUnder(ctx context.Context, prefix string) ([]*Upload, error) {
	rows, err := s.query(ctx, `SELECT `+uploadCols+` FROM uploads WHERE SUBSTR(blob_key, 1, ?) = ?`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
