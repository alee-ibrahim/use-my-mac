package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

// labelFlags collects repeated -label k=v flags.
type labelFlags map[string]string

func (l labelFlags) String() string { return "" }
func (l labelFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return errors.New("labels are key=value")
	}
	l[k] = val
	return nil
}

// enrolCommand mints a one-line install link and prints the command to run
// on the Mac.
func enrolCommand(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("enrol-command", flag.ExitOnError)
	profile := fs.String("profile", "default", "install profile (labels, runtimes) configured on the router")
	ttl := fs.Duration("ttl", time.Hour, "link lifetime (max 168h)")
	uses := fs.Int("uses", 1, "how many Macs may enrol with it (max 1000)")
	note := fs.String("note", "", "note shown in the dashboard")
	labels := labelFlags{}
	fs.Var(labels, "label", "extra label key=value (repeatable)")
	if pos := parseFlags(fs, args); len(pos) != 0 {
		return errors.New("usage: umm enrol-command [-profile P] [-label k=v]... [-ttl 1h] [-uses N] [-note TEXT]")
	}
	t, err := c.CreateInstallToken(ctx, api.InstallTokenRequest{Profile: *profile, Labels: labels,
		TTLMinutes: int(ttl.Minutes()), MaxUses: *uses, Note: *note})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Install link %s (profile %s, %d use(s), expires %s). Run this in Terminal on the Mac:\n\n",
		t.ID, t.Profile.Name, t.MaxUses, t.ExpiresAt.Local().Format(time.RFC1123))
	fmt.Println(t.Command)
	fmt.Fprintf(os.Stderr, "\nRevoke it with: umm install-tokens revoke %s\n", t.ID)
	return nil
}

// installTokens lists, inspects or revokes install links.
func installTokens(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch {
	case args[0] == "list":
		ts, err := c.InstallTokens(ctx)
		if err != nil {
			return err
		}
		w := tw()
		fmt.Fprintln(w, "ID\tSTATE\tPROFILE\tUSES\tEXPIRES\tNOTE")
		for _, t := range ts {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\t%s\t%s\n", t.ID, t.State, t.Profile.Name, t.Uses, t.MaxUses,
				t.ExpiresAt.Local().Format("2006-01-02 15:04"), t.Note)
		}
		return w.Flush()
	case args[0] == "revoke" && len(args) == 2:
		return c.RevokeInstallToken(ctx, args[1])
	case args[0] == "log" && len(args) == 2:
		evs, err := c.InstallTokenEvents(ctx, args[1])
		if err != nil {
			return err
		}
		w := tw()
		fmt.Fprintln(w, "TIME\tEVENT\tIP\tMAC\tDETAIL")
		for _, e := range evs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.At.Local().Format("2006-01-02 15:04:05"), e.Kind, e.IP, e.AgentID, e.Detail)
		}
		return w.Flush()
	}
	return errors.New("usage: umm install-tokens list | log ID | revoke ID")
}
