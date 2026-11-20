package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

func genBytes(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(7, 9))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// fakeS3 is a small path-style S3: presigned requests only, objects and
// multipart uploads in memory, Range on GET. Plain GETs must carry no
// headers SigV4 query signing would need to cover (RGW refuses unsigned
// ones such as Content-Type, and anything x-amz-*), and no Authorization.
type fakeS3 struct {
	mu      sync.Mutex
	objs    map[string][]byte
	uploads map[string]map[int][]byte
	next    int
	gets    []http.Header // object GETs (not multipart listings)
	bad     []string
}

var getHeaderOK = map[string]bool{"Range": true, "User-Agent": true, "Accept-Encoding": true}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("X-Amz-Signature") == "" || q.Get("X-Amz-SignedHeaders") != "host" {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	_, isCreate := q["uploads"]
	uid := q.Get("uploadId")
	switch {
	case r.Method == http.MethodPost && isCreate:
		f.next++
		id := "up" + strconv.Itoa(f.next)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == http.MethodPut && uid != "":
		n, _ := strconv.Atoi(q.Get("partNumber"))
		b, _ := io.ReadAll(r.Body)
		f.uploads[uid][n] = b
		w.Header().Set("ETag", fmt.Sprintf(`"e%d-%d"`, n, len(b)))
	case r.Method == http.MethodGet && uid != "":
		var ns []int
		for n := range f.uploads[uid] {
			ns = append(ns, n)
		}
		sort.Ints(ns)
		fmt.Fprint(w, `<ListPartsResult>`)
		for _, n := range ns {
			b := f.uploads[uid][n]
			fmt.Fprintf(w, `<Part><PartNumber>%d</PartNumber><ETag>"e%d-%d"</ETag><Size>%d</Size></Part>`, n, n, len(b), len(b))
		}
		fmt.Fprint(w, `</ListPartsResult>`)
	case r.Method == http.MethodPost && uid != "":
		var body struct {
			Parts []struct{ PartNumber int } `xml:"Part"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var buf bytes.Buffer
		for _, p := range body.Parts {
			buf.Write(f.uploads[uid][p.PartNumber])
		}
		f.objs[path] = buf.Bytes()
		delete(f.uploads, uid)
		fmt.Fprint(w, `<CompleteMultipartUploadResult></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && uid != "":
		delete(f.uploads, uid)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objs[path] = b
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		if r.Method == http.MethodGet {
			f.gets = append(f.gets, r.Header.Clone())
			for k := range r.Header {
				if !getHeaderOK[k] {
					f.bad = append(f.bad, k)
				}
			}
		}
		b, ok := f.objs[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	case r.Method == http.MethodDelete:
		delete(f.objs, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}

func (f *fakeS3) ranged() (n int, bad []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.gets {
		if h.Get("Range") != "" {
			n++
		}
	}
	return n, append([]string(nil), f.bad...)
}

func hexSum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// With the s3 backend an app downloads an output straight from the bucket
// in parallel ranges on a presigned URL, verifies it and confirms it, which
// counts for after_download. Blobs get presigned URLs too, for their owner.
func TestDirectDownloadsFromS3(t *testing.T) {
	f := &fakeS3{objs: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	s3srv := httptest.NewServer(f)
	defer s3srv.Close()
	e := newEnvWith(t, func(string) storage.Backend {
		return &storage.S3{Endpoint: s3srv.URL, Bucket: "b", AccessKey: "ak", SecretKey: "sk", PathStyle: true, Prefix: "umm/"}
	})
	e.mac("m1", 1, quietTel())
	if me, err := e.app.Me(e.ctx); err != nil || me.Storage != "s3" {
		t.Fatalf("me: %+v %v", me, err)
	}

	const size = 11<<20 + 4321 // three 5 MiB ranges
	want := genBytes(size)
	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Name: "direct", Cleanup: &api.Cleanup{AfterDownload: true},
		Tasks: []api.TaskSpec{{Key: "gen", Runtime: "helper", Args: []string{"gen", strconv.Itoa(size), "{out:big.bin}"},
			Outputs: []api.Output{{Name: "big.bin"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	final, err := e.app.WaitJob(e.ctx, job.ID, 100*time.Millisecond, nil)
	if err != nil || final.State != api.StateSucceeded {
		t.Fatalf("job %v %v", final.State, err)
	}
	full, _ := e.app.Job(e.ctx, job.ID)
	taskID := full.Tasks[0].ID

	d, err := e.app.OutputURL(e.ctx, taskID, "big.bin", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.URL, s3srv.URL+"/b/umm/jobs/") || d.Size != size || d.SHA256 != hexSum(want) ||
		time.Until(d.ExpiresAt) > 10*time.Minute || time.Until(d.ExpiresAt) < 9*time.Minute {
		t.Fatalf("output url: %+v", d)
	}

	dst := filepath.Join(t.TempDir(), "big.bin")
	info, err := e.app.DownloadOutputFileInfo(e.ctx, taskID, "big.bin", dst,
		&client.TransferOptions{Direct: true, PartSize: api.MinPartSize, Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Direct || !info.Verified || !info.Confirmed || info.Size != size {
		t.Fatalf("download info: %+v", info)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, want) {
		t.Fatal("downloaded bytes differ")
	}
	if n, bad := f.ranged(); n < 3 || len(bad) > 0 {
		t.Fatalf("want >= 3 ranged GETs with only safe headers; got %d, unexpected headers %v", n, bad)
	}
	files, err := e.app.Files(e.ctx, client.FileFilter{JobID: job.ID})
	if err != nil || len(files) != 1 || files[0].DownloadedAt == nil {
		t.Fatalf("a confirmed direct download counts as downloaded: %+v %v", files, err)
	}

	// A corrupted object fails the checksum, removes the file and is not
	// confirmed.
	f.mu.Lock()
	for k, b := range f.objs {
		if strings.HasSuffix(k, "/big.bin") {
			c := append([]byte(nil), b...)
			c[100] ^= 0xff
			f.objs[k] = c
		}
	}
	f.mu.Unlock()
	if _, err := e.app.DownloadOutputFileInfo(e.ctx, taskID, "big.bin", dst,
		&client.TransferOptions{Direct: true}); !errors.Is(err, client.ErrChecksum) {
		t.Fatalf("corrupted object: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("a file that fails the checksum must be removed")
	}

	// redirect=1 answers 307 to the presigned URL.
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodGet,
		e.ts.URL+"/v1/tasks/"+taskID+"/outputs/big.bin?direct=1&redirect=1", nil)
	req.Header.Set("Authorization", "Bearer "+e.app.APIKey)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || !strings.HasPrefix(resp.Header.Get("Location"), s3srv.URL) {
		t.Fatalf("redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Blobs: the owner gets a URL that works without the API key; another
	// app key does not.
	if err := e.app.UploadBlob(e.ctx, "it/small.txt", strings.NewReader("hello s3"), 8); err != nil {
		t.Fatal(err)
	}
	bd, err := e.app.BlobURL(e.ctx, "it/small.txt", 0)
	if err != nil || bd.Size != 8 {
		t.Fatalf("blob url: %+v %v", bd, err)
	}
	resp, err = http.Get(bd.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello s3" {
		t.Fatalf("blob via presigned URL: %d %q", resp.StatusCode, body)
	}
	other, err := e.admin.CreateAPIKey(e.ctx, "other-app", api.RoleApp)
	if err != nil {
		t.Fatal(err)
	}
	oc := client.New(e.ts.URL, other.Secret)
	var ce *client.Error
	if _, err := oc.BlobURL(e.ctx, "it/small.txt", 0); !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
		t.Fatalf("other app's blob url: %v", err)
	}
	if _, err := oc.OutputURL(e.ctx, taskID, "big.bin", 0); !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
		t.Fatalf("other app's output url: %v", err)
	}
	if err := oc.ConfirmDownload(e.ctx, taskID, "big.bin", ""); !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
		t.Fatalf("other app's confirm: %v", err)
	}
}
