// Command umm is the use-my-mac CLI: submit work, watch it, fetch results,
// look at the fleet and mint enrolment tokens for new Macs.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

const usage = `umm — use-my-mac CLI

Environment:
  UMM_SERVER         router URL, e.g. https://umm.example.com
  UMM_API_KEY        API key (or UMM_API_KEY_FILE, or ~/.config/umm/api-key)

Commands:
  submit FILE [-wait]            submit a job spec (YAML or JSON); inputs with "file:" are uploaded first
  status [JOB]                   list recent jobs, or show one job and its tasks
  logs TASK [-attempt N]         print a task's logs
  download TASK OUTPUT [-o F]    download a task output
  cancel JOB                     cancel a job
  macs                           list Macs with live telemetry
  drain MAC [-off]               stop sending new work to a Mac (admin)
  enrol [-ttl-hours N]           mint a one-time enrolment token for a new Mac (admin)
  keys list | create NAME [-role app|viewer|admin] | revoke ID   (admin)
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "help" || os.Args[1] == "--help" {
		fmt.Print(usage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func apiKey() (string, error) {
	if v := strings.TrimSpace(os.Getenv("UMM_API_KEY")); v != "" {
		return v, nil
	}
	path := os.Getenv("UMM_API_KEY_FILE")
	if path == "" {
		if h, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(h, ".config", "umm", "api-key")
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("no API key: set UMM_API_KEY, UMM_API_KEY_FILE or ~/.config/umm/api-key")
	}
	return strings.TrimSpace(string(b)), nil
}

func newClient() (*client.Client, error) {
	srv := os.Getenv("UMM_SERVER")
	if srv == "" {
		return nil, errors.New("set UMM_SERVER")
	}
	key, err := apiKey()
	if err != nil {
		return nil, err
	}
	return client.New(srv, key), nil
}

// parseFlags allows flags after positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for len(args) > 0 {
		fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos
}

func run(ctx context.Context, cmd string, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "submit":
		wait := fs.Bool("wait", false, "wait for the job to finish")
		pos := parseFlags(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: umm submit FILE [-wait]")
		}
		return submit(ctx, c, pos[0], *wait)
	case "status":
		pos := parseFlags(fs, args)
		if len(pos) == 0 {
			return listJobs(ctx, c)
		}
		return showJob(ctx, c, pos[0])
	case "logs":
		attempt := fs.Int("attempt", 0, "attempt number (0 = all)")
		pos := parseFlags(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: umm logs TASK [-attempt N]")
		}
		l, err := c.Logs(ctx, pos[0], *attempt)
		if err != nil {
			return err
		}
		fmt.Print(l)
		return nil
	case "download":
		out := fs.String("o", "", "output file (default: OUTPUT name)")
		pos := parseFlags(fs, args)
		if len(pos) != 2 {
			return errors.New("usage: umm download TASK OUTPUT [-o FILE]")
		}
		dst := *out
		if dst == "" {
			dst = filepath.Base(pos[1])
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		n, err := c.DownloadOutput(ctx, pos[0], pos[1], f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", dst, n)
		return nil
	case "cancel":
		pos := parseFlags(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: umm cancel JOB")
		}
		j, err := c.CancelJob(ctx, pos[0])
		if err != nil {
			return err
		}
		fmt.Println(j.ID, j.State)
		return nil
	case "macs":
		return listMacs(ctx, c)
	case "drain":
		off := fs.Bool("off", false, "undrain")
		pos := parseFlags(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: umm drain MAC [-off]")
		}
		v := !*off
		return c.PatchMac(ctx, pos[0], api.MacPatch{Drain: &v})
	case "enrol", "enroll":
		ttl := fs.Int("ttl-hours", 24, "token lifetime")
		parseFlags(fs, args)
		t, err := c.CreateEnrolmentToken(ctx, *ttl)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "One-time enrolment token (expires %s). On the Mac run install.sh and paste it when asked,\nor: sudo -u _umm umm-agent enrol -config /usr/local/etc/umm/agent.yaml\n\n",
			t.ExpiresAt.Local().Format(time.RFC1123))
		fmt.Println(t.Token)
		return nil
	case "keys":
		return keys(ctx, c, args)
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", cmd)
}

// submit reads a YAML/JSON spec. Inputs may use `file: path` which is
// uploaded and rewritten to a blob reference.
func submit(ctx context.Context, c *client.Client, path string, wait bool) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	base := filepath.Dir(path)
	rnd := make([]byte, 6)
	rand.Read(rnd)
	prefix := "cli/" + hex.EncodeToString(rnd)
	tasks, _ := raw["tasks"].([]any)
	for _, ti := range tasks {
		tm, _ := ti.(map[string]any)
		ins, _ := tm["inputs"].([]any)
		for _, ii := range ins {
			im, _ := ii.(map[string]any)
			fp, ok := im["file"].(string)
			if !ok {
				continue
			}
			if !filepath.IsAbs(fp) {
				fp = filepath.Join(base, fp)
			}
			key := prefix + "/" + filepath.Base(fp)
			if err := upload(ctx, c, key, fp); err != nil {
				return err
			}
			delete(im, "file")
			im["blob"] = key
		}
	}
	j, _ := json.Marshal(raw)
	var spec api.JobSpec
	dec := json.NewDecoder(strings.NewReader(string(j)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return fmt.Errorf("job spec: %w", err)
	}
	job, err := c.SubmitJob(ctx, spec)
	if err != nil {
		return err
	}
	fmt.Println(job.ID)
	if !wait {
		return nil
	}
	last := ""
	final, err := c.WaitJob(ctx, job.ID, 2*time.Second, func(j *api.Job) {
		s := fmt.Sprintf("%s %v", j.State, j.Counts)
		if s != last {
			fmt.Fprintln(os.Stderr, time.Now().Format("15:04:05"), s)
			last = s
		}
	})
	if err != nil {
		return err
	}
	if final.State != api.StateSucceeded {
		return fmt.Errorf("job %s", final.State)
	}
	return nil
}

func upload(ctx context.Context, c *client.Client, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "uploading %s (%d bytes)\n", path, st.Size())
	return c.UploadBlob(ctx, key, io.Reader(f), st.Size())
}

func tw() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0) }

func ago(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return time.Since(*t).Round(time.Second).String() + " ago"
}

func listJobs(ctx context.Context, c *client.Client) error {
	js, err := c.Jobs(ctx, "", 50)
	if err != nil {
		return err
	}
	w := tw()
	fmt.Fprintln(w, "JOB\tNAME\tSTATE\tTASKS\tCREATED")
	for _, j := range js {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", j.ID, j.Name, j.State, counts(j.Counts), ago(&j.CreatedAt))
	}
	return w.Flush()
}

func counts(m map[string]int) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func showJob(ctx context.Context, c *client.Client, id string) error {
	j, err := c.Job(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s  %s  %s\n\n", j.ID, j.Name, j.State, counts(j.Counts))
	w := tw()
	fmt.Fprintln(w, "TASK\tKEY\tSTATE\tATTEMPT\tFAILURES\tMAC\tERROR")
	for _, t := range j.Tasks {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n", t.ID, t.Key, t.State, t.Attempt, t.Failures, dash(t.AgentID), t.Error)
	}
	return w.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func listMacs(ctx context.Context, c *client.Client) error {
	ms, err := c.Macs(ctx)
	if err != nil {
		return err
	}
	w := tw()
	fmt.Fprintln(w, "MAC\tNAME\tSTATE\tUSER\tIDLE\tUSER CPU\tOUR CPU\tGPU\tMEM FREE\tPOWER\tTHERMAL\tRUNNING\tFLAGS")
	for _, m := range ms {
		t := m.Telemetry
		if t == nil {
			t = &api.Telemetry{GPUUtilPercent: -1}
		}
		power := "AC"
		if !t.OnAC {
			power = fmt.Sprintf("bat %d%%", t.BatteryPercent)
		}
		gpu := "-"
		if t.GPUUtilPercent >= 0 {
			gpu = fmt.Sprintf("%.0f%%", t.GPUUtilPercent)
		}
		user := dash(t.ConsoleUser)
		if t.ScreenLocked {
			user += " (locked)"
		}
		var flags []string
		if m.Drain {
			flags = append(flags, "drain")
		}
		if m.OptedOut {
			flags = append(flags, "opted-out")
		}
		if m.Quarantined != nil {
			flags = append(flags, "quarantined")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%.0f%%\t%.0f%%\t%s\t%.1f GB\t%s\t%s\t%d/%d\t%s\n",
			m.ID, m.Name, m.AgentState, user, (time.Duration(t.UserIdleSeconds) * time.Second).String(),
			t.UserCPUPercent, t.OurCPUPercent, gpu, float64(t.MemAvailableBytes)/(1<<30), power, dash(t.Thermal),
			len(m.Running), m.MaxTasks, strings.Join(flags, ","))
	}
	return w.Flush()
}

func keys(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	role := fs.String("role", "app", "app, viewer or admin")
	pos := parseFlags(fs, args[1:])
	switch args[0] {
	case "list":
		ks, err := c.APIKeys(ctx)
		if err != nil {
			return err
		}
		w := tw()
		fmt.Fprintln(w, "ID\tNAME\tROLE\tPREFIX\tLAST USED\tREVOKED")
		for _, k := range ks {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s…\t%s\t%v\n", k.ID, k.Name, k.Role, k.Prefix, ago(k.LastUsedAt), k.RevokedAt != nil)
		}
		return w.Flush()
	case "create":
		if len(pos) != 1 {
			return errors.New("usage: umm keys create NAME [-role app|viewer|admin]")
		}
		k, err := c.CreateAPIKey(ctx, pos[0], *role)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "API key (shown once):")
		fmt.Println(k.Secret)
		return nil
	case "revoke":
		if len(pos) != 1 {
			return errors.New("usage: umm keys revoke ID")
		}
		return c.RevokeAPIKey(ctx, pos[0])
	}
	return errors.New("usage: umm keys list|create|revoke")
}
