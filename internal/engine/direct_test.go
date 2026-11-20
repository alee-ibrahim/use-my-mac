package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// s3ish is the local backend under the s3 name, so the engine hands out
// "presigned" (here: HMAC-signed) URLs as it does with S3.
type s3ish struct{ *storage.Local }

func (s3ish) Name() string { return "s3" }

func hexSHA(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// TestDirectDownloads: presigned output and blob URLs for their owner, 400
// on the local backend, and a confirmed download counts for after_download.
func TestDirectDownloads(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	h.put(ownerA, "in.mov", "input-bytes")
	spec := encodeSpec("in.mov")
	spec.Cleanup = &api.Cleanup{AfterDownload: true, KeepInputs: true}
	j := h.submitAs(ownerA, spec)
	h.tick()
	as := h.poll(a)
	body := "result-out.mp4"
	key := OutputKey(j.ID, "t", "out.mp4")
	if err := h.e.blobs.Put(ctx, key, strings.NewReader(body), -1); err != nil {
		t.Fatal(err)
	}
	if err := h.e.TaskUpdate(ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: as[0].Attempt, Outcome: api.OutcomeSucceeded,
		ExitCode: ip(0), Outputs: []string{"out.mp4"}, OutputSHA256: map[string]string{"out.mp4": hexSHA(body)}}); err != nil {
		t.Fatal(err)
	}
	h.tick()

	if _, err := h.e.DirectOutputURL(ctx, as[0].TaskID, "out.mp4", 0); !errors.Is(err, ErrNoDirect) {
		t.Fatalf("local backend: %v", err)
	}
	h.e.blobs = s3ish{h.e.blobs.(*storage.Local)}

	d, err := h.e.DirectOutputURL(ctx, as[0].TaskID, "out.mp4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.URL == "" || d.Size != int64(len(body)) || d.SHA256 != hexSHA(body) ||
		!d.ExpiresAt.Equal(h.now.Add(DefaultDirectDownloadTTL).UTC()) {
		t.Fatalf("direct output: %+v", d)
	}
	if _, err := h.e.DirectOutputURL(ctx, as[0].TaskID, "out.mp4", 8*24*time.Hour); err == nil {
		t.Fatal("a ttl over 7 days must be refused")
	}
	if _, err := h.e.DirectOutputURL(ctx, as[0].TaskID, "nope", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown output: %v", err)
	}

	// Blobs: owner and admin only.
	if d, err := h.e.DirectBlobURL(ctx, ownerA, "in.mov", time.Minute); err != nil || d.Size != int64(len("input-bytes")) {
		t.Fatalf("owner blob: %+v %v", d, err)
	}
	if _, err := h.e.DirectBlobURL(ctx, admin, "in.mov", time.Minute); err != nil {
		t.Fatalf("admin blob: %v", err)
	}
	if _, err := h.e.DirectBlobURL(ctx, ownerB, "in.mov", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner's blob: %v", err)
	}
	if _, err := h.e.DirectBlobURL(ctx, ownerA, "missing.mov", time.Minute); !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing blob: %v", err)
	}

	// Issuing URLs alone never counts as a download.
	h.advance(10 * time.Minute)
	h.e.SweepCleanup(ctx)
	if !h.exists(key) {
		t.Fatal("a presigned URL is not a download")
	}
	// A wrong checksum is refused and not counted.
	if err := h.e.ConfirmDownload(ctx, as[0].TaskID, "out.mp4", hexSHA("other")); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("bad checksum: %v", err)
	}
	if b, _ := h.e.st.GetBlob(ctx, key); b.DownloadedAt != 0 {
		t.Fatal("a refused confirmation must not count")
	}
	if err := h.e.ConfirmDownload(ctx, as[0].TaskID, "out.mp4", strings.ToUpper(hexSHA(body))); err != nil {
		t.Fatal(err)
	}
	h.e.SweepCleanup(ctx)
	if !h.exists(key) {
		t.Fatal("cleanup waits for the quiet period after a confirmation")
	}
	h.advance(cleanupGrace)
	h.e.SweepCleanup(ctx)
	if h.exists(key) {
		t.Fatal("a confirmed direct download must let after_download clean up")
	}
	if !h.exists(UploadKey("in.mov")) {
		t.Fatal("keep_inputs")
	}
}
