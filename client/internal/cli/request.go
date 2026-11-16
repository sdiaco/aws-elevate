package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

// pollDelays is the client polling backoff: 2, 4, 8 s, then every 8 s.
var pollDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// RoleName is the ephemeral permission set of a grant, which is the role the user gets.
func RoleName(grantID string) string { return "JIT-" + grantID }

func runRequest(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "request")
	account := f.String("account", "", "target account, id or name")
	duration := f.String("duration", "", "how long, for example 45m or 2h (default: the rule's)")
	reason := f.String("reason", "", "why you need it")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("one rule at a time, got %s: %w", strings.Join(rest, " "), errUsage)
	}
	seconds, err := parseDuration(*duration)
	if err != nil {
		return fmt.Errorf("--duration: %v: %w", err, errUsage)
	}
	profile := f.profileName(e)
	b, svc, err := connect(ctx, e, profile)
	if err != nil {
		return err
	}
	rules, err := svc.Rules(ctx)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		printRules(e, rules.Rules)
		return nil
	}

	req := api.CreateRequest{
		RuleID:          rest[0],
		Account:         accountID(rules.Rules, rest[0], *account),
		DurationSeconds: seconds,
		Reason:          *reason,
	}
	g, err := svc.CreateGrant(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Stderr, "Request %s created.\n", g.ID)
	if g, err = waitActive(ctx, e, svc, g); err != nil {
		return err
	}

	role := RoleName(g.ID)
	fmt.Fprintf(e.Stderr, "Grant %s active until %s; waiting for AWS to accept the role…\n", g.ID, when(g.ExpiresAt))
	if err := b.WaitReady(ctx, g.Account, role); err != nil {
		return err
	}
	name, err := awsx.WriteProfile(e.ConfigFile, b.Source(), g.SourcePs.Name, g.Account, role)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Stderr, "Ready. Use it with aws elevate shell, or run:\n")
	fmt.Fprintf(e.Stdout, "export AWS_PROFILE=%s\n", name)
	return nil
}

// waitActive polls the grant until the service activates or closes it. A pending request
// waits for its approvers, up to the service's own timeout.
func waitActive(ctx context.Context, e *Env, svc Service, g *api.Grant) (*api.Grant, error) {
	told := ""
	for attempt := 0; ; attempt++ {
		switch g.State {
		case api.StateActive:
			return g, nil
		case api.StateClosed:
			return nil, fmt.Errorf("request %s closed: %s", g.ID, closeText(g))
		case api.StatePending:
			if told != g.State {
				fmt.Fprintf(e.Stderr, "Waiting for approval by %s. Ctrl-C stops waiting; the request stays pending (aws elevate status).\n", approvers(g))
			}
		case api.StateActivating:
			if told != g.State {
				fmt.Fprintln(e.Stderr, "Activating…")
			}
		default:
			return nil, fmt.Errorf("request %s is %s: see aws elevate status", g.ID, g.State)
		}
		told = g.State
		if err := e.Sleep(ctx, pollDelays[min(attempt, len(pollDelays)-1)]); err != nil {
			return nil, fmt.Errorf("stopped waiting for %s: %w", g.ID, err)
		}
		d, err := svc.Grant(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		g = &d.Grant
	}
}

// accountID turns an account name of the rule into its id; anything else goes to the
// service as typed, which decides whether it is allowed.
func accountID(rules []api.Rule, ruleID, account string) string {
	for _, r := range rules {
		if r.ID != ruleID {
			continue
		}
		for _, a := range r.Accounts {
			if a.Name == account {
				return a.ID
			}
		}
	}
	return account
}

func printRules(e *Env, rules []api.Rule) {
	if len(rules) == 0 {
		fmt.Fprintln(e.Stdout, "No rule lets you request access. Ask an administrator.")
		return
	}
	w := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RULE\tPERMISSION SET\tACCOUNTS\tDURATION\tAPPROVAL")
	for _, r := range rules {
		accounts := make([]string, len(r.Accounts))
		for i, a := range r.Accounts {
			accounts[i] = a.Name + " (" + a.ID + ")"
		}
		approval := r.Approval
		if r.Privileged {
			approval += ", privileged"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s (max %s)\t%s\n", r.ID, r.PermissionSet.Name, strings.Join(accounts, ", "),
			short(r.Duration.DefaultSeconds), short(r.Duration.MaxSeconds), approval)
	}
	w.Flush()
	fmt.Fprintln(e.Stdout, "\nRequest with: aws elevate request <rule> --account <id|name> --reason \"…\" [--duration 45m]")
}

// parseDuration reads 45m, 2h or 1h30m; empty means the rule default.
func parseDuration(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration like 45m or 2h", s)
	}
	if d <= 0 || d%time.Second != 0 {
		return 0, fmt.Errorf("%q must be a positive number of whole seconds", s)
	}
	return int(d / time.Second), nil
}

func short(seconds int) string {
	d := time.Duration(seconds) * time.Second
	s := d.String() // 1h0m0s, 45m0s, 30s
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func approvers(g *api.Grant) string {
	if g.ApproverGroup == nil {
		return "an approver"
	}
	return "group " + g.ApproverGroup.Name
}

func closeText(g *api.Grant) string {
	text := "closed"
	if g.CloseReason != nil {
		text = *g.CloseReason
	}
	if g.Error != nil {
		text += " (" + *g.Error + ")"
	}
	return text
}

func when(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("15:04 Mon 2 Jan")
}
