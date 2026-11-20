package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

// sizeFlag parses 8MiB, 16M, 1GiB, 1048576.
type sizeFlag int64

func (s *sizeFlag) String() string { return strconv.FormatInt(int64(*s), 10) }

func (s *sizeFlag) Set(v string) error {
	n, err := parseSize(v)
	if err != nil {
		return err
	}
	*s = sizeFlag(n)
	return nil
}

func parseSize(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToUpper(v))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(v, u.suf) {
			v, mult = strings.TrimSpace(strings.TrimSuffix(v, u.suf)), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("bad size %q (e.g. 8MiB)", v)
	}
	return int64(f * float64(mult)), nil
}

// transferFlags adds -part-size and -concurrency.
type transferFlags struct {
	partSize    sizeFlag
	concurrency int
	direct      bool
}

func addTransferFlags(fs *flag.FlagSet) *transferFlags {
	t := &transferFlags{partSize: api.DefaultPartSize}
	fs.Var(&t.partSize, "part-size", "multipart part size (min 5MiB)")
	fs.IntVar(&t.concurrency, "concurrency", 4, "parts transferred in parallel")
	fs.BoolVar(&t.direct, "direct", false, "upload parts straight to S3 (presigned) when the router uses S3")
	return t
}

func (t *transferFlags) options(label string) (*client.TransferOptions, func()) {
	if t.partSize < api.MinPartSize {
		fmt.Fprintf(os.Stderr, "part size raised to the 5MiB minimum\n")
		t.partSize = api.MinPartSize
	}
	bar := newBar(label)
	return &client.TransferOptions{PartSize: int64(t.partSize), Concurrency: t.concurrency, Direct: t.direct,
		OnProgress: bar.update}, bar.done
}

// bar prints transfer progress on stderr: an updating line on a terminal,
// a line every few seconds otherwise.
type bar struct {
	label string
	tty   bool
	mu    sync.Mutex
	last  time.Time
	cur   client.Progress
}

func newBar(label string) *bar {
	return &bar{label: label, tty: term.IsTerminal(int(os.Stderr.Fd()))}
}

func (b *bar) update(p client.Progress) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur = p
	if b.tty {
		fmt.Fprintf(os.Stderr, "\r\033[K%s %s", b.label, p)
		return
	}
	if time.Since(b.last) >= 5*time.Second {
		b.last = time.Now()
		fmt.Fprintf(os.Stderr, "%s %s\n", b.label, p)
	}
}

func (b *bar) done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.cur
	avg := 0.0
	if s := p.Elapsed.Seconds(); s > 0 {
		avg = float64(p.Done) / s / (1 << 20)
	}
	if b.tty {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	fmt.Fprintf(os.Stderr, "%s %.1f MiB in %s (%.1f MiB/s)\n", b.label, float64(p.Done)/(1<<20),
		p.Elapsed.Round(100*time.Millisecond), avg)
}

// uploadCmd: umm upload FILE [KEY] [-part-size 8MiB] [-concurrency 4] [-resume ID]
func uploadCmd(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("upload", flag.ExitOnError)
	tf := addTransferFlags(fs)
	resume := fs.String("resume", "", "resume an interrupted multipart upload by ID")
	pos := parseFlags(fs, args)
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: umm upload FILE [KEY] [-part-size 8MiB] [-concurrency 4] [-resume UPLOAD_ID]")
	}
	key := ""
	if len(pos) == 2 {
		key = pos[1]
	} else {
		key = "cli/" + randHex() + "/" + filepath.Base(pos[0])
	}
	opt, done := tf.options("upload " + filepath.Base(pos[0]))
	opt.OnUpload = func(id string) { fmt.Fprintf(os.Stderr, "multipart upload %s (resume with -resume %s)\n", id, id) }
	var err error
	if *resume != "" {
		f, ferr := os.Open(pos[0])
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		st, serr := c.UploadStatus(ctx, *resume)
		if serr != nil {
			return serr
		}
		key = st.Key
		err = c.ResumeUpload(ctx, *resume, f, opt)
	} else {
		err = c.UploadFile(ctx, key, pos[0], opt)
	}
	done()
	if err != nil {
		return err
	}
	fmt.Println(key)
	return nil
}

// downloadCmd: umm download TASK OUTPUT [-o FILE] [-part-size 8MiB] [-concurrency 4]
func downloadCmd(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	out := fs.String("o", "", "output file (default: OUTPUT name)")
	noVerify := fs.Bool("no-verify", false, "skip the SHA-256 check against the Mac's checksum")
	tf := addTransferFlags(fs)
	pos := parseFlags(fs, args)
	if len(pos) != 2 {
		return errors.New("usage: umm download TASK OUTPUT [-o FILE] [-part-size 8MiB] [-concurrency 4]")
	}
	dst := *out
	if dst == "" {
		dst = filepath.Base(pos[1])
	}
	opt, done := tf.options("download " + filepath.Base(dst))
	n, err := c.DownloadOutputFile(ctx, pos[0], pos[1], dst, opt)
	done()
	if err != nil {
		return err
	}
	if !*noVerify {
		switch err := c.VerifyOutputFile(ctx, pos[0], pos[1], dst); {
		case errors.Is(err, client.ErrNoChecksum):
		case err != nil:
			return err
		default:
			fmt.Fprintf(os.Stderr, "wrote %s (%d bytes, SHA-256 verified)\n", dst, n)
			return nil
		}
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", dst, n)
	return nil
}

// watch follows a job live until it finishes.
func watch(ctx context.Context, c *client.Client, id string) (*api.Job, error) {
	tty := term.IsTerminal(int(os.Stderr.Fd()))
	last := ""
	return c.WaitJob(ctx, id, 2*time.Second, func(j *api.Job) {
		s := jobLine(j)
		if s == last {
			return
		}
		last = s
		if tty {
			fmt.Fprintf(os.Stderr, "\r\033[K%s", s)
			if api.IsTerminal(j.State) {
				fmt.Fprintln(os.Stderr)
			}
			return
		}
		fmt.Fprintln(os.Stderr, time.Now().Format("15:04:05"), s)
	})
}

// jobLine: "running 43.5% (running) 2/5 done · 1 running".
func jobLine(j *api.Job) string {
	total, done := 0, 0
	for st, n := range j.Counts {
		total += n
		if api.IsTerminal(st) {
			done += n
		}
	}
	s := fmt.Sprintf("%s %.1f%%", j.State, j.Percent)
	if j.Phase != "" && j.Phase != j.State && j.Phase != api.PhaseDone {
		s += " (" + j.Phase + ")"
	}
	s += fmt.Sprintf(" %d/%d tasks done", done, total)
	for _, t := range j.Tasks {
		if t.Progress != nil && t.Progress.BytesPerSec > 0 {
			s += fmt.Sprintf(" · %s %s %.1f MiB/s", t.Key, t.Phase, t.Progress.BytesPerSec/(1<<20))
		}
	}
	return s
}
