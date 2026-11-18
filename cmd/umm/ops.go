package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

// Operations commands: fleet summary, activity log, retention and cleanup.

func pct(v float64) string {
	if v < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

func metricsCmd(ctx context.Context, c *client.Client, args []string) error {
	s, err := c.Summary(ctx)
	if err != nil {
		return err
	}
	m := s.Macs
	fmt.Printf("Macs     %d total · %d online · %d busy · %d idle · %d yielded · %d draining · %d offline\n",
		m["total"], m["online"], m["busy"], m["idle"], m["yielded"], m["draining"], m["offline"])
	fmt.Printf("Tasks    %d running · %d queued · %d blocked\n", s.Tasks["running"], s.Tasks["queued"], s.Tasks["blocked"])
	fmt.Printf("Jobs     %d running · %d queued\n", s.Jobs["running"], s.Jobs["queued"])
	w := tw()
	fmt.Fprintln(w, "\nWINDOW\tSUCCEEDED\tFAILED\tTIMEOUT\tYIELDED\tLOST\tSUCCESS\tCOMPUTE H")
	for _, x := range []struct {
		n string
		w api.Window
	}{{"24h", s.Last24h}, {"7d", s.Last7d}} {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%s\t%.1f\n", x.n, x.w.Succeeded, x.w.Failed, x.w.Timeout, x.w.Yielded,
			x.w.Lost, pct(x.w.SuccessRate), x.w.ComputeHours)
	}
	return w.Flush()
}

func eventsCmd(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	kind := fs.String("kind", "", "comma-separated kinds; a trailing dot matches a prefix (mac.,key.)")
	limit := fs.Int("limit", 50, "max events")
	parseFlags(fs, args)
	var kinds []string
	if *kind != "" {
		kinds = strings.Split(*kind, ",")
	}
	evs, err := c.Events(ctx, kinds, *limit)
	if err != nil {
		return err
	}
	w := tw()
	fmt.Fprintln(w, "TIME\tKIND\tBY\tMESSAGE")
	for _, e := range evs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.At.Local().Format("2006-01-02 15:04:05"), e.Kind, dash(e.Actor), e.Message)
	}
	return w.Flush()
}

func retentionCmd(ctx context.Context, c *client.Client, args []string) error {
	cur, err := c.Retention(ctx)
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "set" {
		fs := flag.NewFlagSet("retention", flag.ExitOnError)
		jobs := fs.Int("jobs-days", cur.JobsDays, "delete finished jobs after N days (0 = keep)")
		events := fs.Int("events-days", cur.EventsDays, "keep the activity log N days")
		raw := fs.Int("raw-hours", cur.MetricsRawHours, "keep 1-minute metrics N hours (24-168)")
		rollup := fs.Int("rollup-days", cur.MetricsRollupDays, "keep 15-minute metrics N days")
		parseFlags(fs, args[1:])
		cur, err = c.SetRetention(ctx, api.Retention{JobsDays: *jobs, EventsDays: *events, MetricsRawHours: *raw,
			MetricsRollupDays: *rollup})
		if err != nil {
			return err
		}
	} else if len(args) > 0 && args[0] != "show" {
		return errors.New("usage: umm retention [show | set -jobs-days N -events-days N -raw-hours N -rollup-days N]")
	}
	jobs := "keep"
	if cur.JobsDays > 0 {
		jobs = fmt.Sprintf("%d days", cur.JobsDays)
	}
	fmt.Printf("finished jobs   %s\nactivity log    %d days\nmetrics 1 min   %d hours\nmetrics 15 min  %d days\n",
		jobs, cur.EventsDays, cur.MetricsRawHours, cur.MetricsRollupDays)
	return nil
}

func jobsCmd(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return listJobs(ctx, c)
	}
	switch args[0] {
	case "delete":
		if len(args) < 2 {
			return errors.New("usage: umm jobs delete JOB...")
		}
		for _, id := range args[1:] {
			if err := c.DeleteJob(ctx, id); err != nil {
				return fmt.Errorf("%s: %w", id, err)
			}
			fmt.Println("deleted", id)
		}
		return nil
	case "purge":
		fs := flag.NewFlagSet("jobs purge", flag.ExitOnError)
		older := fs.String("older-than", "", "finished at least this long ago, e.g. 30d (required)")
		state := fs.String("state", "", "only these states (succeeded,failed,cancelled)")
		parseFlags(fs, args[1:])
		days, err := parseDays(*older)
		if err != nil {
			return err
		}
		req := api.PurgeJobsRequest{OlderThanDays: days}
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
	return errors.New("usage: umm jobs list | delete JOB... | purge -older-than 30d [-state failed]")
}

func parseDays(v string) (int, error) {
	if v == "" {
		return 0, errors.New("-older-than is required (e.g. 30d; 0d = every finished job)")
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		var d int
		if _, err := fmt.Sscanf(n, "%d", &d); err == nil && d >= 0 {
			return d, nil
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad duration %q (use days, e.g. 30d)", v)
	}
	return int(d.Hours() / 24), nil
}

func macsCmd(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return listMacs(ctx, c)
	}
	if args[0] == "delete" && len(args) >= 2 {
		for _, id := range args[1:] {
			if err := c.DeleteMac(ctx, id); err != nil {
				return fmt.Errorf("%s: %w", id, err)
			}
			fmt.Println("deleted", id, "(its credential no longer works)")
		}
		return nil
	}
	return errors.New("usage: umm macs [list | delete MAC...]")
}
