package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

// Stored files, job removal for owners, and router-side Mac settings.

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

const filesUsage = `usage: umm files [ls] [-kind upload|output] [-job JOB] [-prefix P] [-owner OWNER|*]
       umm files rm KEY...                        (a storage key uploads/... or jobs/..., or a blob key)
       umm files rm -all [-kind K] [-job JOB] [-prefix P] [-owner OWNER|*]
                                                  every matching file you own; files in use are skipped`

func filesCmd(ctx context.Context, c *client.Client, args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("files "+sub, flag.ExitOnError)
	kind := fs.String("kind", "", "upload or output")
	job := fs.String("job", "", "only this job's files")
	prefix := fs.String("prefix", "", "storage key prefix, e.g. uploads/myapp/")
	owner := fs.String("owner", "", "admins: another owner's files, or * for everyone's")
	all := fs.Bool("all", false, "rm: every matching file (with the filters above)")
	limit := fs.Int("limit", 1000, "ls: max files")
	pos := parseFlags(fs, args)
	switch sub {
	case "ls", "list":
		files, err := c.Files(ctx, client.FileFilter{Kind: *kind, JobID: *job, Prefix: *prefix, Owner: *owner, Limit: *limit})
		if err != nil {
			return err
		}
		w := tw()
		fmt.Fprintln(w, "KEY\tKIND\tSIZE\tJOB\tSTORED\tDOWNLOADED\tIN USE")
		var total int64
		for _, f := range files {
			total += f.Size
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%v\n", f.Key, f.Kind, humanBytes(f.Size), dash(f.JobID),
				ago(&f.CreatedAt), ago(f.DownloadedAt), f.InUse)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Printf("\n%d files, %s\n", len(files), humanBytes(total))
		return nil
	case "rm", "delete":
		req := api.DeleteFilesRequest{Keys: pos, Kind: *kind, JobID: *job, Prefix: *prefix, Owner: *owner, All: *all}
		if len(pos) == 0 && !*all {
			return errors.New(filesUsage)
		}
		if len(pos) > 0 && *all {
			return errors.New("give keys or -all, not both")
		}
		r, err := c.DeleteFiles(ctx, req)
		if err != nil {
			return err
		}
		fmt.Printf("deleted %d files (%s)\n", r.Deleted, humanBytes(r.Bytes))
		for _, s := range r.Skipped {
			fmt.Printf("  kept %s: %s\n", s.Key, s.Reason)
		}
		return nil
	}
	return errors.New(filesUsage)
}

// jobsRm deletes jobs: by ID (cancelling running ones), or every finished
// job (optionally by state and age) with -finished.
func jobsRm(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("jobs rm", flag.ExitOnError)
	finished := fs.Bool("finished", false, "delete every finished job of yours (admins: everyone's, or -owner)")
	state := fs.String("state", "", "with -finished: only these states (succeeded,failed,cancelled)")
	older := fs.String("older-than", "0d", "with -finished: finished at least this long ago, e.g. 7d")
	owner := fs.String("owner", "", "with -finished, admins: only this owner's jobs")
	inputs := fs.Bool("inputs", false, "also delete the input files you uploaded for them (unless still in use)")
	pos := parseFlags(fs, args)
	if *finished {
		if len(pos) > 0 {
			return errors.New("give job IDs or -finished, not both")
		}
		days, err := parseDays(*older)
		if err != nil {
			return err
		}
		req := api.PurgeJobsRequest{OlderThanDays: days, Inputs: *inputs, Owner: *owner}
		if *state != "" {
			req.States = strings.Split(*state, ",")
		}
		n, err := c.PurgeJobs(ctx, req)
		if err != nil {
			return err
		}
		fmt.Printf("deleted %d jobs\n", n)
		return nil
	}
	if len(pos) == 0 {
		return errors.New("usage: umm jobs rm JOB... [-inputs] | umm jobs rm -finished [-state S] [-older-than 7d] [-inputs]")
	}
	for _, id := range pos {
		if err := c.RemoveJob(ctx, id, *inputs); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		fmt.Println("deleted", id)
	}
	return nil
}

// resolveMac accepts a Mac ID or a unique name.
func resolveMac(ctx context.Context, c *client.Client, ref string) (*api.Mac, error) {
	ms, err := c.Macs(ctx)
	if err != nil {
		return nil, err
	}
	var hits []api.Mac
	for _, m := range ms {
		if m.ID == ref {
			return &m, nil
		}
		if m.Name == ref {
			hits = append(hits, m)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no Mac %q", ref)
	case 1:
		return &hits[0], nil
	}
	return nil, fmt.Errorf("several Macs are named %q; use the ID", ref)
}

func macLabelCmd(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("macs label", flag.ExitOnError)
	var rm multiFlag
	fs.Var(&rm, "rm", "remove a router label (repeatable)")
	clear := fs.Bool("clear", false, "remove every router label first")
	pos := parseFlags(fs, args)
	if len(pos) < 1 {
		return errors.New("usage: umm macs label MAC [k=v ...] [-rm KEY] [-clear]")
	}
	m, err := resolveMac(ctx, c, pos[0])
	if err != nil {
		return err
	}
	labels := map[string]string{}
	if !*clear {
		for k, v := range m.AdminLabels {
			labels[k] = v
		}
	}
	for _, kv := range pos[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("label %q must be key=value", kv)
		}
		labels[k] = v
	}
	for _, k := range rm {
		delete(labels, k)
	}
	if len(pos) == 1 && len(rm) == 0 && !*clear {
		printLabels("agent labels", m.AgentLabels)
		printLabels("router labels", m.AdminLabels)
		return nil
	}
	if err := c.SetMacLabels(ctx, m.ID, labels); err != nil {
		return err
	}
	printLabels(m.Name+" router labels", labels)
	return nil
}

func printLabels(title string, m map[string]string) {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, k+"="+m[k])
	}
	fmt.Printf("%s: %s\n", title, dash(strings.Join(parts, " ")))
}

func macReserveCmd(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("macs reserve", flag.ExitOnError)
	forOwners := fs.String("for", "", "comma-separated owners: API key IDs (key_...) or user:<subject> (required)")
	hours := fs.Int("hours", 0, "end the reservation after N hours")
	until := fs.String("until", "", "end the reservation at this time (RFC 3339)")
	note := fs.String("note", "", "why (shown on the dashboard)")
	pos := parseFlags(fs, args)
	if len(pos) != 1 || *forOwners == "" {
		return errors.New("usage: umm macs reserve MAC -for key_abc[,key_def] [-hours N | -until 2026-12-01T00:00:00Z] [-note TEXT]")
	}
	m, err := resolveMac(ctx, c, pos[0])
	if err != nil {
		return err
	}
	req := api.MacReservationRequest{Owners: strings.Split(*forOwners, ","), Hours: *hours, Note: *note}
	if *until != "" {
		t, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			return fmt.Errorf("-until: %w", err)
		}
		req.Until = &t
	}
	if err := c.ReserveMac(ctx, m.ID, req); err != nil {
		return err
	}
	fmt.Println("reserved", m.Name, "for", *forOwners)
	return nil
}

func macMaxTasksCmd(ctx context.Context, c *client.Client, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: umm macs max-tasks MAC N   (0 = the agent's own max_tasks)")
	}
	n, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("N: %w", err)
	}
	m, err := resolveMac(ctx, c, args[0])
	if err != nil {
		return err
	}
	return c.SetMacMaxTasks(ctx, m.ID, n)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
