package agent

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLineParserUMMProgress(t *testing.T) {
	p := &lineParser{}
	cases := map[string]float64{
		"UMM_PROGRESS=42":         42,
		"  UMM_PROGRESS=99.5  ":   99.5,
		"step 3 UMM_PROGRESS=7%":  7,
		"UMM_PROGRESS=100":        100,
		"frame=1 time=00:00:01.0": -1, // not ffmpeg mode
		"UMM_PROGRESS=abc":        -1,
		"XUMM_PROGRESS=5":         -1,
	}
	for line, want := range cases {
		v, ok := p.parse(line)
		if want < 0 {
			if ok {
				t.Errorf("%q: unexpected %v", line, v)
			}
			continue
		}
		if !ok || v != want {
			t.Errorf("%q: got %v %v, want %v", line, v, ok, want)
		}
	}
}

func TestLineParserFFmpegStats(t *testing.T) {
	p := &lineParser{ffmpeg: true}
	feed := []struct {
		line string
		want float64 // -1 = no value
	}{
		{"frame=   10 fps=0.0 q=0.0 size=       0kB time=00:00:01.00 bitrate=N/A", -1}, // no duration yet
		{"Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'in/video.mp4':", -1},
		{"  Duration: 00:01:40.00, start: 0.000000, bitrate: 1205 kb/s", -1},
		{"  Duration: 00:09:00.00, start: 0.000000, bitrate: 1 kb/s", -1}, // second input ignored
		{"frame=  250 fps=50 q=28.0 size=    1024kB time=00:00:25.00 bitrate= 335.5kbits/s speed=5x", 25},
		{"frame=  500 fps=50 q=28.0 size=    2048kB time=00:00:50.00 bitrate= 335.5kbits/s speed=5x", 50},
		{"out_time_us=75000000", 75},
		{"out_time_ms=80000000", 80},
		{"out_time=00:01:30.000000", 90},
		{"progress=end", 100},
	}
	for _, f := range feed {
		v, ok := p.parse(f.line)
		if f.want < 0 {
			if ok {
				t.Errorf("%q: unexpected %v", f.line, v)
			}
			continue
		}
		if !ok || math.Abs(v-f.want) > 0.01 {
			t.Errorf("%q: got %v %v, want %v", f.line, v, ok, f.want)
		}
	}
}

func TestProgressTapSplitsCarriageReturns(t *testing.T) {
	var mu sync.Mutex
	var got []float64
	tap := &progressTap{p: &lineParser{ffmpeg: true}, report: func(v float64) { mu.Lock(); got = append(got, v); mu.Unlock() }}
	tap.Write([]byte("  Duration: 00:00:10.00, start: 0\n"))
	tap.Write([]byte("frame=1 time=00:00:02.50 bitrate=x\rframe=2 time=00:00:0"))
	tap.Write([]byte("5.00 bitrate=x\rUMM_PROGRESS=60\n"))
	if len(got) != 3 || got[0] != 25 || got[1] != 50 || got[2] != 60 {
		t.Fatalf("got %v", got)
	}
}

func TestParserFor(t *testing.T) {
	c := &Config{Runtimes: map[string][]string{
		"ffmpeg": {"/opt/homebrew/bin/ffmpeg", "-hide_banner"},
		"enc":    {"/usr/local/bin/ffmpeg7"},
		"sh":     {"/bin/sh"},
		"wrap":   {"/usr/bin/python3"},
	}, ProgressParsers: map[string]string{"enc": ParserNone, "wrap": ParserFFmpeg}}
	for rt, want := range map[string]bool{"ffmpeg": true, "enc": false, "sh": false, "wrap": true} {
		if got := c.parserFor(rt).ffmpeg; got != want {
			t.Errorf("%s: ffmpeg=%v want %v", rt, got, want)
		}
	}
	bad := &Config{Server: "http://x", WorkDir: "/w", Runtimes: map[string][]string{"sh": {"/bin/sh"}},
		ProgressParsers: map[string]string{"sh": "magic"}}
	if err := bad.Normalize(); err == nil {
		t.Fatal("unknown parser must fail")
	}
	ok := &Config{Server: "http://x", WorkDir: "/w", Runtimes: map[string][]string{"sh": {"/bin/sh"}}}
	if err := ok.Normalize(); err != nil || ok.Transfer.PartSize() != 8<<20 || ok.Transfer.Concurrency != 4 {
		t.Fatalf("defaults: %v %+v", err, ok.Transfer)
	}
}

func TestWatchProgressFile(t *testing.T) {
	old := progressInterval
	progressInterval = 20 * time.Millisecond
	defer func() { progressInterval = old }()
	p := filepath.Join(t.TempDir(), ProgressFileName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan float64, 10)
	go watchProgressFile(ctx, p, func(v float64) { ch <- v })
	os.WriteFile(p, []byte("33.3\n"), 0o600)
	select {
	case v := <-ch:
		if v != 33.3 {
			t.Fatalf("got %v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("progress file not read")
	}
}
