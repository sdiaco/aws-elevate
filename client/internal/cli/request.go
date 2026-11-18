package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/term"
)

// pollDelays is the client polling backoff: 2, 4, 8 s, then every 8 s.
var pollDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// RoleName is the ephemeral permission set of a grant, which is the role the user gets.
func RoleName(grantID string) string { return "JIT-" + grantID }

func runRequest(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "request")
	account := f.String("account", "", "target account, id or name (default: the only account of the rule)")
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
	rules, err := svc.RequestableRules(ctx)
	if err != nil {
		return err
	}
	var req api.CreateRequest
	switch {
	case len(rest) == 1:
		acc, err := accountID(rules.Rules, rest[0], *account)
		if err != nil {
			return err
		}
		req = api.CreateRequest{RuleID: rest[0], Account: acc, DurationSeconds: seconds, Reason: *reason}
	case !e.UI().Interactive() || len(rules.Rules) == 0:
		printRules(e, rules.Rules)
		return nil
	default:
		if req, err = askRequest(e.UI(), rules.Rules, *account, *duration, *reason); err != nil {
			return err
		}
	}
	g, err := svc.CreateGrant(ctx, req)
	if err != nil {
		return err
	}
	if g.State == api.StatePending {
		fmt.Fprintf(e.Stderr, "Request %s is waiting for approval by %s; it expires %s.\n"+
			"Once approved: aws elevate shell %s\n", g.ID, approvers(g), inMinutes(g.ApprovalExpiresAt), g.ID)
		return nil
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
	fmt.Fprintf(e.Stderr, "Ready. Use it with aws elevate shell, or add to your aws commands:\n")
	fmt.Fprintf(e.Stdout, "--profile %s\n", name)
	return nil
}

// waitActive polls an activating grant until the service activates or closes it.
func waitActive(ctx context.Context, e *Env, svc Service, g *api.Grant) (*api.Grant, error) {
	fmt.Fprintln(e.Stderr, "Activating…")
	for attempt := 0; ; attempt++ {
		switch g.State {
		case api.StateActive:
			return g, nil
		case api.StateClosed:
			return nil, fmt.Errorf("request %s closed: %s", g.ID, closeText(g))
		case api.StateActivating:
		default:
			return nil, fmt.Errorf("request %s is %s: see aws elevate status", g.ID, g.State)
		}
		if err := e.Sleep(ctx, pollDelays[min(attempt, len(pollDelays)-1)]); err != nil {
			// Leaving the wait does not withdraw the request: the service keeps activating it.
			fmt.Fprintf(e.Stderr, "The request %s stays open: aws elevate status\n", g.ID)
			return nil, fmt.Errorf("stopped waiting for %s: %w", g.ID, err)
		}
		d, err := svc.Grant(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		g = &d.Grant
	}
}

// askRequest asks what the flags did not give: the rule, the account when the rule has more
// than one, the duration and the reason. The service decides whether the request is allowed.
func askRequest(u *term.UI, rules []api.Rule, account, duration, reason string) (api.CreateRequest, error) {
	items := make([]term.Item, len(rules))
	for i, r := range rules {
		items[i] = term.Item{Title: r.ID, Detail: ruleDetail(r)}
	}
	i, err := u.Select("Rule", items, false)
	if err != nil {
		return api.CreateRequest{}, err
	}
	r := rules[i]
	req := api.CreateRequest{RuleID: r.ID}
	if req.Account, err = askAccount(u, r, account); err != nil {
		return req, err
	}
	if duration == "" {
		if duration, err = u.Ask("Duration, empty for "+short(r.Duration.DefaultSeconds), ""); err != nil {
			return req, err
		}
	}
	if req.DurationSeconds, err = parseDuration(duration); err != nil {
		return req, fmt.Errorf("duration: %w", err)
	}
	req.Reason, err = askReason(u, reason)
	return req, err
}

func askAccount(u *term.UI, r api.Rule, account string) (string, error) {
	if account != "" || len(r.Accounts) == 1 {
		return accountID([]api.Rule{r}, r.ID, account)
	}
	items := make([]term.Item, len(r.Accounts))
	for i, a := range r.Accounts {
		items[i] = term.Item{Title: a.Name, Detail: a.ID}
	}
	i, err := u.Select("Account", items, false)
	if err != nil {
		return "", err
	}
	return r.Accounts[i].ID, nil
}

// askReason asks until there is a reason, a few times: the end of the input answers empty.
func askReason(u *term.UI, reason string) (string, error) {
	for range 3 {
		if reason = strings.TrimSpace(reason); reason != "" {
			return reason, nil
		}
		var err error
		if reason, err = u.Ask("Reason (approvers and the audit read it)", ""); err != nil {
			return "", err
		}
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		return reason, nil
	}
	return "", fmt.Errorf("a reason is required: %w", errUsage)
}

func ruleDetail(r api.Rule) string {
	accounts := make([]string, len(r.Accounts))
	for i, a := range r.Accounts {
		accounts[i] = a.Name
	}
	approval := r.Approval
	if r.Privileged {
		approval += ", privileged"
	}
	return fmt.Sprintf("%s on %s · %s · %s", r.PermissionSet.Name, strings.Join(accounts, ", "), short(r.Duration.DefaultSeconds), approval)
}

// accountID turns an account name of the rule into its id; anything else goes to the
// service as typed, which decides whether it is allowed. Without --account, the only
// account of the rule is the one meant, as in the interface.
func accountID(rules []api.Rule, ruleID, account string) (string, error) {
	for _, r := range rules {
		if r.ID != ruleID {
			continue
		}
		if account == "" {
			if len(r.Accounts) == 1 {
				return r.Accounts[0].ID, nil
			}
			names := make([]string, len(r.Accounts))
			for i, a := range r.Accounts {
				names[i] = a.Name + " (" + a.ID + ")"
			}
			return "", fmt.Errorf("%s has several accounts: add --account with one of %s: %w", ruleID, strings.Join(names, ", "), errUsage)
		}
		for _, a := range r.Accounts {
			if a.Name == account {
				return a.ID, nil
			}
		}
	}
	return account, nil
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
	fmt.Fprintln(e.Stdout, "\nRequest with: aws elevate request <rule> --reason \"…\" [--account <id|name>] [--duration 45m]")
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

// inMinutes is "in 30 min (10:30)", rounded to the nearest minute.
func inMinutes(t *time.Time) string {
	if t == nil {
		return "when the service times it out"
	}
	left := time.Until(*t).Round(time.Minute)
	return fmt.Sprintf("in %d min (%s)", int(left.Minutes()), t.Local().Format("15:04"))
}

func when(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("15:04 Mon 2 Jan")
}
