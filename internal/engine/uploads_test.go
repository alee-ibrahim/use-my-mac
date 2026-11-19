package engine

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func TestNormalizePartSize(t *testing.T) {
	if ps, err := normalizePartSize(100<<20, 0); err != nil || ps != api.DefaultPartSize {
		t.Fatalf("default: %d %v", ps, err)
	}
	if _, err := normalizePartSize(100<<20, 1<<20); err == nil {
		t.Fatal("parts below 5 MiB must be rejected")
	}
	if _, err := normalizePartSize(0, 0); err == nil {
		t.Fatal("empty uploads must be rejected")
	}
	// 200 GiB at 8 MiB would be 25,600 parts: grow to whole MiB that fit.
	ps, err := normalizePartSize(200<<30, 0)
	if err != nil || api.PartCount(200<<30, ps) > api.MaxParts || ps%(1<<20) != 0 {
		t.Fatalf("grown part size %d (%d parts): %v", ps, api.PartCount(200<<30, ps), err)
	}
}

func TestRelayUploadLifecycle(t *testing.T) {
	h := newHarness(t)
	const ps = api.MinPartSize
	data := bytes.Repeat([]byte("0123456789abcdef"), (2*ps+1000)/16)
	size := int64(len(data))
	up, err := h.e.CreateUpload(h.ctx, "app1", api.UploadRequest{Key: "app/big.bin", Size: size, PartSize: ps})
	if err != nil {
		t.Fatal(err)
	}
	if up.Parts != 3 || up.Key != "app/big.bin" || len(up.URLs) != 3 || up.URLs[2] != RelayPartPath(up.ID, 2) {
		t.Fatalf("upload: %+v", up)
	}
	part := func(n int) []byte {
		off := int64(n-1) * ps
		return data[off : off+api.PartSizeOf(size, ps, n)]
	}
	// Parts arrive out of order; a wrong-sized part is rejected.
	if err := h.e.PutUploadPart(h.ctx, up.ID, "app1", false, 3, bytes.NewReader(part(3)), int64(len(part(3)))); err != nil {
		t.Fatal(err)
	}
	var ve *ValidationError
	if err := h.e.PutUploadPart(h.ctx, up.ID, "app1", false, 1, strings.NewReader("short"), 5); !errors.As(err, &ve) {
		t.Fatalf("short part: %v", err)
	}
	if err := h.e.PutUploadPart(h.ctx, up.ID, "app1", false, 4, strings.NewReader("x"), 1); !errors.As(err, &ve) {
		t.Fatalf("part out of range: %v", err)
	}
	// Other owners cannot see or use it.
	if _, err := h.e.GetUpload(h.ctx, up.ID, "app2", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner: %v", err)
	}
	if _, err := h.e.CompleteUpload(h.ctx, up.ID, "app1", false); !errors.As(err, &ve) {
		t.Fatalf("complete with missing parts: %v", err)
	}
	// Resume: GET lists what is stored and URLs for the rest.
	st, err := h.e.GetUpload(h.ctx, up.ID, "app1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Done) != 1 || st.Done[0] != 3 || len(st.URLs) != 2 {
		t.Fatalf("status: %+v", st)
	}
	for _, n := range []int{1, 2} {
		if err := h.e.PutUploadPart(h.ctx, up.ID, "app1", false, n, bytes.NewReader(part(n)), -1); err != nil {
			t.Fatal(err)
		}
	}
	// Admins may finish app uploads.
	if _, err := h.e.CompleteUpload(h.ctx, up.ID, "admin", true); err != nil {
		t.Fatal(err)
	}
	rc, err := h.e.blobs.Get(h.ctx, UploadKey("app/big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("assembled blob differs (%d vs %d bytes)", len(got), len(data))
	}
	if _, err := h.e.GetUpload(h.ctx, up.ID, "app1", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished upload still there: %v", err)
	}
}

func TestOutputUploadAndExpiry(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{{Key: "t", Runtime: "sh", Outputs: []api.Output{{Name: "out.bin"}}}}})
	h.tick()
	as := h.poll(a)
	if len(as) != 1 || len(as[0].Outputs) != 1 || !as[0].Outputs[0].Multipart {
		t.Fatalf("assignment: %+v", as)
	}
	req := api.OutputUploadRequest{Attempt: as[0].Attempt, Size: 2 * api.MinPartSize}
	if _, err := h.e.CreateOutputUpload(h.ctx, a, as[0].TaskID, "missing", req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown output: %v", err)
	}
	if _, err := h.e.CreateOutputUpload(h.ctx, "agt_other", as[0].TaskID, "out.bin", req); !errors.Is(err, ErrStale) {
		t.Fatalf("other agent: %v", err)
	}
	up, err := h.e.CreateOutputUpload(h.ctx, a, as[0].TaskID, "out.bin", req)
	if err != nil {
		t.Fatal(err)
	}
	// Agents get signed URLs, never router relay paths.
	for n, u := range up.URLs {
		if strings.HasPrefix(u, "/") {
			t.Fatalf("part %d: relay URL %q for an agent", n, u)
		}
	}
	if up.Key != OutputKey(j.ID, "t", "out.bin") {
		t.Fatalf("key %q", up.Key)
	}
	// Apps cannot touch agent uploads, even as admin.
	if _, err := h.e.GetUpload(h.ctx, up.ID, "admin", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admin on agent upload: %v", err)
	}

	// Never completed: the sweep aborts it once it expires.
	h.advance(UploadTTL + time.Minute)
	h.e.ExpireUploads(h.ctx)
	if _, err := h.e.st.GetUpload(h.ctx, up.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired upload kept: %v", err)
	}
	if err := h.e.CompleteAgentUpload(h.ctx, a, up.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("complete after expiry: %v", err)
	}
}
