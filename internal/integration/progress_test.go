package integration

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

// A big input goes up in parallel parts, the task reports progress (a
// UMM_PROGRESS line, then the progress file) that the app sees live on the
// job stream, the big output comes back as a multipart upload from the
// agent, and the app downloads it in parallel ranges.
func TestMultipartTransfersAndLiveProgress(t *testing.T) {
	e := newEnv(t)
	e.mac("m1", 1, quietTel())

	data := make([]byte, 11<<20+12345) // three 5 MiB parts
	r := rand.New(rand.NewPCG(1, 2))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	src := filepath.Join(t.TempDir(), "in.bin")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var upProgress []client.Progress
	var uploadID string
	err := e.app.UploadFile(e.ctx, "it/in.bin", src, &client.TransferOptions{PartSize: api.MinPartSize, Concurrency: 3,
		ProgressEvery: 10 * time.Millisecond,
		OnUpload:      func(id string) { uploadID = id },
		OnProgress: func(p client.Progress) {
			mu.Lock()
			upProgress = append(upProgress, p)
			mu.Unlock()
		}})
	if err != nil {
		t.Fatal(err)
	}
	if uploadID == "" {
		t.Fatal("a file bigger than one part must use a multipart upload")
	}
	if last := upProgress[len(upProgress)-1]; last.Done != int64(len(data)) || last.Total != int64(len(data)) {
		t.Fatalf("final upload progress %+v", last)
	}

	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Tasks: []api.TaskSpec{{Key: "big", Runtime: "helper",
		Args:    []string{"big", "{in:src}", "{out:out.bin}"},
		Inputs:  []api.Input{{Name: "src", Blob: "it/in.bin"}},
		Outputs: []api.Output{{Name: "out.bin"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]float64{} // phase -> highest task percent seen
	var jobPct []float64
	final, err := e.app.WatchJob(e.ctx, job.ID, time.Second, func(j *api.Job) {
		jobPct = append(jobPct, j.Percent)
		for _, tk := range j.Tasks {
			if tk.Phase != "" {
				seen[tk.Phase] = max(seen[tk.Phase], tk.Percent)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if final.State != api.StateSucceeded || final.Percent != 100 || final.Phase != api.PhaseDone {
		t.Fatalf("final job: %s %v %s", final.State, final.Percent, final.Phase)
	}
	// 40% of the running band is 5 + 90*0.4 = 41; 80% is 77.
	if seen[api.PhaseRunning] < 77 {
		t.Fatalf("live running progress not seen: %v (job percents %v)", seen, jobPct)
	}
	for i := 1; i < len(jobPct); i++ {
		if jobPct[i] < jobPct[i-1] {
			t.Fatalf("job percent went backwards: %v", jobPct)
		}
	}

	full, err := e.app.Job(e.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out.bin")
	n, err := e.app.DownloadOutputFile(e.ctx, full.Tasks[0].ID, "out.bin", dst, &client.TransferOptions{PartSize: api.MinPartSize})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	want := bytes.Clone(data)
	for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
		want[i], want[j] = want[j], want[i]
	}
	if n != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("output differs: %d bytes", n)
	}
	// The finished upload is gone: resuming it fails.
	if _, err := e.app.UploadStatus(e.ctx, uploadID); err == nil {
		t.Fatal("completed upload still listed")
	}
}
