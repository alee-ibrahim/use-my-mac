// Example: split a video locally, encode the chunks in parallel on the Mac
// pool with VideoToolbox, then concatenate them on one Mac (map -> reduce).
//
//	export UMM_SERVER=https://umm.example.com UMM_API_KEY=...   # app key
//	go run ./examples/video-transcode -in input.mov -out output.mp4 -chunk 60
//
// Requirements: ffmpeg on the machine running this example (for splitting)
// and an "ffmpeg" runtime allowlisted on the Macs:
//
//	runtimes:
//	  ffmpeg: ["/opt/homebrew/bin/ffmpeg", "-hide_banner", "-nostdin", "-y"]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

func main() {
	in := flag.String("in", "", "input video")
	out := flag.String("out", "output.mp4", "output file")
	chunk := flag.Int("chunk", 60, "chunk length in seconds")
	codec := flag.String("codec", "hevc_videotoolbox", "encoder on the Macs (hevc_videotoolbox, h264_videotoolbox, ...)")
	bitrate := flag.String("bitrate", "8M", "video bitrate")
	flag.Parse()
	if *in == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	c := client.New(os.Getenv("UMM_SERVER"), os.Getenv("UMM_API_KEY"))

	// 1. Split without re-encoding (fast, local).
	tmp, err := os.MkdirTemp("", "umm-split-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	split := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", *in, "-map", "0", "-c", "copy",
		"-f", "segment", "-segment_time", fmt.Sprint(*chunk), "-reset_timestamps", "1",
		filepath.Join(tmp, "chunk%04d"+filepath.Ext(*in)))
	split.Stdout, split.Stderr = os.Stdout, os.Stderr
	if err := split.Run(); err != nil {
		log.Fatalf("split: %v", err)
	}
	chunks, _ := filepath.Glob(filepath.Join(tmp, "chunk*"))
	sort.Strings(chunks)
	log.Printf("split into %d chunks", len(chunks))

	// 2. Upload chunks and build the job.
	prefix := fmt.Sprintf("video/%d", time.Now().UnixNano())
	reduce := api.TaskSpec{Key: "concat", Runtime: "ffmpeg", Outputs: []api.Output{{Name: "final.mp4"}},
		Resources: api.Resources{RAMMB: 1024}}
	var concat []string
	var tasks []api.TaskSpec
	for i, p := range chunks {
		key := fmt.Sprintf("%s/%s", prefix, filepath.Base(p))
		f, err := os.Open(p)
		if err != nil {
			log.Fatal(err)
		}
		st, _ := f.Stat()
		if err := c.UploadBlob(ctx, key, f, st.Size()); err != nil {
			log.Fatalf("upload %s: %v", p, err)
		}
		f.Close()
		k := fmt.Sprintf("enc-%04d", i)
		args := []string{"-i", "{in:src}", "-c:v", *codec, "-b:v", *bitrate}
		if strings.HasPrefix(*codec, "hevc") {
			args = append(args, "-tag:v", "hvc1")
		}
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-f", "mpegts", "{out:part.ts}")
		tasks = append(tasks, api.TaskSpec{
			Key: k, Runtime: "ffmpeg",
			Args:           args,
			Inputs:         []api.Input{{Name: "src", Blob: key}},
			Outputs:        []api.Output{{Name: "part.ts"}},
			Resources:      api.Resources{RAMMB: 2048, GPU: true, ExpectedSeconds: *chunk},
			TimeoutSeconds: 30 * 60,
		})
		in := fmt.Sprintf("p%04d", i)
		reduce.Inputs = append(reduce.Inputs, api.Input{Name: in, From: &api.OutputRef{Task: k, Output: "part.ts"}})
		concat = append(concat, "{in:"+in+"}")
	}
	reduce.Args = []string{"-i", "concat:" + strings.Join(concat, "|"), "-c", "copy",
		"-bsf:a", "aac_adtstoasc", "-movflags", "+faststart", "{out:final.mp4}"}
	tasks = append(tasks, reduce)

	job, err := c.SubmitJob(ctx, api.JobSpec{Name: "transcode " + filepath.Base(*in), Tasks: tasks})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("submitted %s", job.ID)

	// 3. Wait and download.
	final, err := c.WaitJob(ctx, job.ID, 3*time.Second, func(j *api.Job) {
		log.Printf("%s %v", j.State, j.Counts)
	})
	if err != nil || final.State != api.StateSucceeded {
		log.Fatalf("job ended %v: %v", final.State, err)
	}
	full, _ := c.Job(ctx, job.ID)
	for _, t := range full.Tasks {
		if t.Key == "concat" {
			f, err := os.Create(*out)
			if err != nil {
				log.Fatal(err)
			}
			if _, err := c.DownloadOutput(ctx, t.ID, "final.mp4", f); err != nil {
				log.Fatal(err)
			}
			f.Close()
			log.Printf("wrote %s", *out)
		}
	}
}
