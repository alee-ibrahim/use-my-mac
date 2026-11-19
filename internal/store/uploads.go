package store

import (
	"context"
	"database/sql"
	"errors"
)

// Multipart uploads in progress. The bytes live in the storage backend; this
// table only maps our upload ID to the backend's and records who may finish
// it. Rows are deleted on complete/abort or when they expire.

func init() {
	schema = append(schema,
		`CREATE TABLE IF NOT EXISTS uploads (
			id TEXT PRIMARY KEY,
			owner TEXT NOT NULL,
			blob_key TEXT NOT NULL,
			backend_id TEXT NOT NULL,
			size BIGINT NOT NULL,
			part_size BIGINT NOT NULL,
			direct INTEGER NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			expires_at BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS uploads_expires ON uploads(expires_at)`,
	)
}

// Upload is a row of uploads.
type Upload struct {
	ID        string
	Owner     string
	Key       string // full backend key (e.g. uploads/<app key>)
	BackendID string
	Size      int64
	PartSize  int64
	Direct    bool
	CreatedAt int64
	ExpiresAt int64
}

const uploadCols = `id, owner, blob_key, backend_id, size, part_size, direct, created_at, expires_at`

func scanUpload(sc interface{ Scan(...any) error }) (*Upload, error) {
	var u Upload
	var direct int
	err := sc.Scan(&u.ID, &u.Owner, &u.Key, &u.BackendID, &u.Size, &u.PartSize, &direct, &u.CreatedAt, &u.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.Direct = direct != 0
	return &u, err
}

// CreateUpload inserts an upload.
func (s *Store) CreateUpload(ctx context.Context, u *Upload) error {
	_, err := s.exec(ctx, `INSERT INTO uploads(`+uploadCols+`) VALUES(?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Owner, u.Key, u.BackendID, u.Size, u.PartSize, b2i(u.Direct), u.CreatedAt, u.ExpiresAt)
	return err
}

// GetUpload by ID.
func (s *Store) GetUpload(ctx context.Context, id string) (*Upload, error) {
	return scanUpload(s.queryRow(ctx, `SELECT `+uploadCols+` FROM uploads WHERE id = ?`, id))
}

// DeleteUpload removes an upload row (missing rows are not an error).
func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM uploads WHERE id = ?`, id)
	return err
}

// ExpiredUploads lists uploads that expired before now.
func (s *Store) ExpiredUploads(ctx context.Context, now int64, limit int) ([]*Upload, error) {
	rows, err := s.query(ctx, `SELECT `+uploadCols+` FROM uploads WHERE expires_at < ? ORDER BY expires_at LIMIT ?`, now, limit)
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
