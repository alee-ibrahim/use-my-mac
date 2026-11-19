package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/transfer"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Live progress of a running attempt. The agent reports the phase
// (downloading inputs, running, uploading outputs) and a percentage to the
// router about once a second while it changes. While the process runs the
// percentage comes from the task itself, through any of:
//
//   - a line "UMM_PROGRESS=<0-100>" on stdout or stderr;
//   - the file $UMM_PROGRESS_FILE, rewritten with a number from 0 to 100;
//   - a built-in parser for the runtime (ffmpeg: "Duration:" and "time="
//     from its normal stats, or out_time_us= from -progress).
//
// Tasks that report nothing simply show "running" without a percentage.

// ProgressFileName is the file (in the task's work dir) tasks may write
// their percentage to; its path is in $UMM_PROGRESS_FILE.
const ProgressFileName = "progress"

// Progress parsers selectable per runtime in the config.
const (
	ParserAuto   = "auto" // ffmpeg for runtimes whose executable is ffmpeg
	ParserFFmpeg = "ffmpeg"
	ParserNone   = "none" // only UMM_PROGRESS lines and the progress file
)

// progressReporter keeps the latest progress of one attempt and sends it.
type progressReporter struct {
	a       *Agent
	taskID  string
	attempt int

	mu      sync.Mutex
	cur     api.TaskProgress
	dirty   bool
	lastAt  time.Time
	disable bool // the router does not know the endpoint (old version)

	done chan struct{}
	wg   sync.WaitGroup
}

// progressInterval is how often a changed report is sent.
var progressInterval = time.Second

func newProgressReporter(a *Agent, taskID string, attempt int) *progressReporter {
	p := &progressReporter{a: a, taskID: taskID, attempt: attempt, done: make(chan struct{}),
		cur: api.TaskProgress{Attempt: attempt, Phase: api.PhaseDownloading, Percent: -1}}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(progressInterval)
		defer t.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-t.C:
				p.flush(false)
			}
		}
	}()
	return p
}

// phase switches phase and resets the numbers.
func (p *progressReporter) phase(phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cur = api.TaskProgress{Attempt: p.attempt, Phase: phase, Percent: -1}
	p.dirty = true
}

// percent sets the in-phase percentage (running phase).
func (p *progressReporter) percent(v float64) {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur.Percent != v {
		p.cur.Percent, p.dirty = v, true
	}
}

// bytes sets transfer numbers (downloading/uploading phases).
func (p *progressReporter) bytes(s transfer.Stats) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cur.BytesDone, p.cur.BytesTotal, p.cur.BytesPerSec = s.Done, s.Total, s.BytesPerSec
	p.cur.Percent = s.Percent()
	p.dirty = true
}

// flush sends the report if it changed (or every 15 s as a keep-alive).
func (p *progressReporter) flush(force bool) {
	p.mu.Lock()
	if p.disable || (!p.dirty && !force && time.Since(p.lastAt) < 15*time.Second) {
		p.mu.Unlock()
		return
	}
	cur := p.cur
	p.dirty, p.lastAt = false, time.Now()
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := p.a.call(ctx, http.MethodPost, "/v1/agent/tasks/"+url.PathEscape(p.taskID)+"/progress", cur, nil)
	if err != nil && (strings.Contains(err.Error(), " 404 ") || strings.Contains(err.Error(), " 405 ")) {
		p.mu.Lock()
		p.disable = true
		p.mu.Unlock()
		p.a.log.Debug("router does not take progress reports", "err", err)
	} else if err != nil {
		p.a.log.Debug("progress report failed", "task", p.taskID, "err", err)
	}
}

func (p *progressReporter) close() {
	close(p.done)
	p.wg.Wait()
}

// ---------------------------------------------------------------------------
// Parsing task output
// ---------------------------------------------------------------------------

var (
	ummProgressRe = regexp.MustCompile(`(?:^|\s)UMM_PROGRESS=([0-9]+(?:\.[0-9]+)?)\s*%?\s*$`)
	ffDurationRe  = regexp.MustCompile(`Duration:\s*(\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)
	ffTimeRe      = regexp.MustCompile(`(?:^|\s)time=\s*(\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)
	ffOutTimeRe   = regexp.MustCompile(`^out_time_(?:us|ms)=(\d+)$`)
	ffOutTimeHMS  = regexp.MustCompile(`^out_time=(\d+):(\d{2}):(\d{2}(?:\.\d+)?)$`)
)

func hms(m []string) float64 {
	h, _ := strconv.ParseFloat(m[1], 64)
	mi, _ := strconv.ParseFloat(m[2], 64)
	s, _ := strconv.ParseFloat(m[3], 64)
	return h*3600 + mi*60 + s
}

// lineParser turns task output lines into a percentage.
type lineParser struct {
	ffmpeg   bool
	duration float64 // seconds, from the first "Duration:" line
}

// parse returns a percentage and true when the line carries one.
func (l *lineParser) parse(line string) (float64, bool) {
	line = strings.TrimSpace(line)
	if m := ummProgressRe.FindStringSubmatch(line); m != nil {
		v, err := strconv.ParseFloat(m[1], 64)
		return v, err == nil
	}
	if !l.ffmpeg {
		return 0, false
	}
	if l.duration == 0 {
		if m := ffDurationRe.FindStringSubmatch(line); m != nil {
			l.duration = hms(m)
			return 0, false
		}
	}
	if line == "progress=end" {
		return 100, true
	}
	if l.duration <= 0 {
		return 0, false
	}
	var cur float64
	switch {
	case ffOutTimeRe.MatchString(line):
		us, _ := strconv.ParseFloat(ffOutTimeRe.FindStringSubmatch(line)[1], 64)
		cur = us / 1e6
	case ffOutTimeHMS.MatchString(line):
		cur = hms(ffOutTimeHMS.FindStringSubmatch(line))
	default:
		m := ffTimeRe.FindStringSubmatch(line)
		if m == nil {
			return 0, false
		}
		cur = hms(m)
	}
	return min(100, 100*cur/l.duration), true
}

// progressTap is an io.Writer that splits output into lines (on \n and \r,
// which ffmpeg uses between stats updates) and feeds the parser.
type progressTap struct {
	p      *lineParser
	report func(float64)

	mu  sync.Mutex
	buf []byte
}

const maxLine = 4096

func (t *progressTap) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range b {
		if c == '\n' || c == '\r' {
			t.line()
			continue
		}
		if len(t.buf) < maxLine {
			t.buf = append(t.buf, c)
		}
	}
	return len(b), nil
}

func (t *progressTap) line() {
	if len(t.buf) == 0 {
		return
	}
	if v, ok := t.p.parse(string(t.buf)); ok {
		t.report(v)
	}
	t.buf = t.buf[:0]
}

// parserFor picks the output parser for a runtime. argv is the command the
// task runs (an allowlisted or managed runtime); "auto" looks at its program.
func (c *Config) parserFor(runtime string, argv []string) *lineParser {
	mode := c.ProgressParsers[runtime]
	if mode == "" {
		mode = ParserAuto
	}
	switch mode {
	case ParserFFmpeg:
		return &lineParser{ffmpeg: true}
	case ParserAuto:
		if len(argv) > 0 && strings.HasPrefix(strings.ToLower(filepath.Base(argv[0])), "ffmpeg") {
			return &lineParser{ffmpeg: true}
		}
	}
	return &lineParser{}
}

// watchProgressFile polls $UMM_PROGRESS_FILE until ctx ends.
func watchProgressFile(ctx context.Context, path string, report func(float64)) {
	t := time.NewTicker(progressInterval / 2)
	defer t.Stop()
	var last []byte
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b, err := os.ReadFile(path)
		if err != nil || len(b) > 64 || bytes.Equal(b, last) {
			continue
		}
		last = b
		s := strings.TrimSuffix(strings.TrimSpace(string(b)), "%")
		if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			report(v)
		}
	}
}
