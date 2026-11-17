package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

// exitCode is the exit status of a command that ran something else (the shell).
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

func runStatus(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "status")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	_, svc, err := connect(ctx, e, f.profileName(e))
	if err != nil {
		return err
	}
	mine, err := listAll(ctx, svc, api.ListQuery{Scope: "mine", Open: true})
	if err != nil {
		return err
	}
	approvable, err := listAll(ctx, svc, api.ListQuery{Scope: "approvable"})
	if err != nil {
		return err
	}
	toApprove := slices.DeleteFunc(approvable, func(g api.Grant) bool { return !slices.Contains(g.Actions, "approve") })

	if len(mine) == 0 {
		fmt.Fprintln(e.Stdout, "You have no open grant or request.")
	} else {
		fmt.Fprintln(e.Stdout, "Your grants and requests:")
		printGrants(e, mine)
	}
	if len(toApprove) > 0 {
		fmt.Fprintln(e.Stdout, "\nTo approve (aws elevate approve <id> [--reject]):")
		printGrants(e, toApprove)
	}
	return nil
}

func listAll(ctx context.Context, svc Service, q api.ListQuery) ([]api.Grant, error) {
	var all []api.Grant
	for {
		page, err := svc.Grants(ctx, q)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.Cursor == nil {
			return all, nil
		}
		q.Cursor = *page.Cursor
	}
}

func printGrants(e *Env, grants []api.Grant) {
	w := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  ID\tSTATE\tRULE\tPERMISSION SET\tACCOUNT\tREQUESTER\tUNTIL\tREASON")
	for _, g := range grants {
		rule := g.RuleID
		if g.RuleRemoved {
			rule += " (removed)"
		}
		state := g.State
		if g.Privileged {
			state += ", privileged"
		}
		until := g.ExpiresAt
		if g.State == api.StatePending {
			until = g.ApprovalExpiresAt // when the request times out
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", g.ID, state, rule, g.SourcePs.Name, g.Account,
			g.Requester.UserName, when(until), g.Reason)
	}
	w.Flush()
}

// runRevoke ends grants early, or cancels pending requests: which one is the action the
// service offers on each grant.
func runRevoke(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "revoke")
	reason := f.String("reason", "", "why the access ends early (not needed to cancel a request)")
	ids, err := f.parse(args)
	if err != nil {
		return err
	}
	_, svc, err := connect(ctx, e, f.profileName(e))
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		g, err := onlyOpen(ctx, svc, canEnd)
		if err != nil {
			return err
		}
		ids = []string{g.ID}
	}
	failed := 0
	for _, id := range ids {
		if err := revokeOne(ctx, svc, id, *reason, e); err != nil {
			fmt.Fprintf(e.Stderr, "%s: %v\n", id, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d not done", failed, len(ids))
	}
	return nil
}

func revokeOne(ctx context.Context, svc Service, id, reason string, e *Env) error {
	d, err := svc.Grant(ctx, id)
	if err != nil {
		return err
	}
	var g *api.Grant
	if slices.Contains(d.Grant.Actions, "cancel") {
		g, err = svc.Cancel(ctx, id)
	} else {
		g, err = svc.Revoke(ctx, id, reason)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "%s: %s\n", id, stateText(g))
	return nil
}

func runApprove(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "approve")
	reject := f.Bool("reject", false, "reject the request instead")
	reason := f.String("reason", "", "why the request is rejected (required with --reject)")
	ids, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(ids) != 1 {
		return fmt.Errorf("give exactly one request id: %w", errUsage)
	}
	*reason = strings.TrimSpace(*reason)
	if *reject && *reason == "" {
		return fmt.Errorf("--reject needs --reason \"why\": the requester reads it: %w", errUsage)
	}
	if !*reject && *reason != "" {
		return fmt.Errorf("--reason goes with --reject: %w", errUsage)
	}
	_, svc, err := connect(ctx, e, f.profileName(e))
	if err != nil {
		return err
	}
	var g *api.Grant
	if *reject {
		g, err = svc.Reject(ctx, ids[0], *reason)
	} else {
		g, err = svc.Approve(ctx, ids[0])
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "%s: %s\n", g.ID, stateText(g))
	return nil
}

// runShell starts $SHELL with the profile of an active grant (design §10.3).
func runShell(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "shell")
	ids, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(ids) > 1 {
		return fmt.Errorf("one grant at a time: %w", errUsage)
	}
	b, svc, err := connect(ctx, e, f.profileName(e))
	if err != nil {
		return err
	}
	var g *api.Grant
	if len(ids) == 1 {
		d, err := svc.Grant(ctx, ids[0])
		if err != nil {
			return err
		}
		g = &d.Grant
		if g.State != api.StateActive {
			return fmt.Errorf("grant %s is %s, not active", g.ID, g.State)
		}
	} else if g, err = onlyOpen(ctx, svc, isActive); err != nil {
		return err
	}
	// The grant may have just become active (after an approval): wait until AWS accepts the
	// role. The profile may come from another machine or the UI: write it again, it is stable.
	if err := b.WaitReady(ctx, g.Account, RoleName(g.ID)); err != nil {
		return err
	}
	name, err := awsx.WriteProfile(e.ConfigFile, b.Source(), g.SourcePs.Name, g.Account, RoleName(g.ID))
	if err != nil {
		return err
	}
	shell := e.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	fmt.Fprintf(e.Stderr, "Shell with AWS_PROFILE=%s until %s; exit to leave.\n", name, when(g.ExpiresAt))
	code, err := e.Shell(shell, shellEnv(e.Environ(), name))
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// onlyOpen picks the caller's one open grant that keep accepts; with none or several it
// says what to pass.
func onlyOpen(ctx context.Context, svc Service, keep func(api.Grant) bool) (*api.Grant, error) {
	mine, err := listAll(ctx, svc, api.ListQuery{Scope: "mine", Open: true})
	if err != nil {
		return nil, err
	}
	found := slices.DeleteFunc(mine, func(g api.Grant) bool { return !keep(g) })
	switch len(found) {
	case 0:
		return nil, errors.New("no active grant: request one with aws elevate request")
	case 1:
		return &found[0], nil
	}
	ids := make([]string, len(found))
	for i, g := range found {
		ids[i] = g.ID + " (" + g.SourcePs.Name + " on " + g.Account + ")"
	}
	return nil, fmt.Errorf("more than one open grant: %s; pass the id", strings.Join(ids, ", "))
}

func isActive(g api.Grant) bool { return g.State == api.StateActive }

func canEnd(g api.Grant) bool {
	return slices.Contains(g.Actions, "revoke") || slices.Contains(g.Actions, "cancel")
}

func stateText(g *api.Grant) string {
	if g.State == api.StateClosed {
		return "closed, " + closeText(g)
	}
	return g.State
}

// shellEnv is the current environment with the grant profile. Variables that would take
// precedence over AWS_PROFILE are dropped, so the shell really uses the grant.
func shellEnv(environ []string, profile string) []string {
	drop := []string{"AWS_PROFILE=", "AWS_DEFAULT_PROFILE=", "AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN="}
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		return slices.ContainsFunc(drop, func(p string) bool { return strings.HasPrefix(kv, p) })
	})
	return append(env, "AWS_PROFILE="+profile)
}
