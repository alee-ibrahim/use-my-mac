package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

const accessUsage = `usage: umm access SUBCMD (admin)
  rules                          list role rules (config rules are locked)
  users                          people who signed in: role and why
  set group|user VALUE admin|viewer [-name NAME]
                                 add or change a rule (user VALUE: subject, object ID, email or UPN)
  user SUBJECT admin|viewer|none set or remove a signed-in user's own rule
  rm RULE_ID                     remove a dashboard rule
  default deny|viewer            role for signed-in users no rule matches
  name GROUP_ID NAME             display name for a group ID ("" removes it)
  log                            audit log of role changes`

// accessCmd manages dashboard roles for single-sign-on users.
func accessCmd(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 {
		args = []string{"rules"}
	}
	switch {
	case args[0] == "rules":
		p, err := c.Access(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Default role: %s (config %s, dashboard %s)\n\n", p.DefaultRole, p.ConfigDefaultRole, p.UIDefaultRole)
		w := tw()
		fmt.Fprintln(w, "ID\tKIND\tVALUE\tNAME\tROLE\tSOURCE")
		for _, r := range p.Rules {
			src := r.Source
			if r.Locked {
				src += " (locked)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Kind, r.Value, dash(r.Name), r.Role, src)
		}
		return w.Flush()
	case args[0] == "users":
		us, err := c.AccessUsers(ctx)
		if err != nil {
			return err
		}
		w := tw()
		fmt.Fprintln(w, "SUBJECT\tNAME\tEMAIL\tLAST LOGIN\tROLE\tWHY")
		for _, u := range us {
			role := u.Role
			if role == "" {
				role = "denied"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", u.Subject, u.Name, dash(u.Email),
				u.LastLogin.Local().Format("2006-01-02 15:04"), role, u.Reason)
		}
		return w.Flush()
	case args[0] == "set" && (len(args) == 4 || (len(args) == 6 && args[4] == "-name")):
		name := ""
		if len(args) == 6 {
			name = args[5]
		}
		r, err := c.PutAccessRule(ctx, accessRuleRequest(args[1], args[2], args[3], name))
		if err != nil {
			return err
		}
		fmt.Printf("%s %s -> %s (rule %s)\n", r.Kind, r.Value, r.Role, r.ID)
		return nil
	case args[0] == "user" && len(args) == 3:
		role := args[2]
		if role == "none" {
			role = ""
		}
		return c.SetAccessUserRole(ctx, args[1], role)
	case args[0] == "rm" && len(args) == 2:
		return c.DeleteAccessRule(ctx, args[1])
	case args[0] == "default" && len(args) == 2:
		p, err := c.SetAccessDefault(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Default role: %s\n", p.DefaultRole)
		return nil
	case args[0] == "name" && len(args) == 3:
		return c.SetAccessGroupName(ctx, args[1], args[2])
	case args[0] == "log":
		evs, err := c.AccessAudit(ctx)
		if err != nil {
			return err
		}
		w := tw()
		fmt.Fprintln(w, "TIME\tACTOR\tACTION\tDETAIL")
		for _, e := range evs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.At.Local().Format(time.DateTime), e.Actor, e.Action, e.Detail)
		}
		return w.Flush()
	}
	return errors.New(accessUsage)
}

func accessRuleRequest(kind, value, role, name string) api.AccessRuleRequest {
	return api.AccessRuleRequest{Kind: strings.ToLower(kind), Value: value, Role: strings.ToLower(role), Name: name}
}
