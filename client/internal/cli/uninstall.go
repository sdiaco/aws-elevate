package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/term"
)

// Uninstaller is what the uninstall calls on AWS with the account admin's own profile.
type Uninstaller interface {
	Check(ctx context.Context) (awsx.Installation, error)
	Leftovers(ctx context.Context) (awsx.Leftovers, error)
	DeleteStack(ctx context.Context) error
	Stack(ctx context.Context, stackID string) (awsx.StackState, error)
	DeleteTable(ctx context.Context) error
	DeleteRules(ctx context.Context, appID string) error
	DeleteOrphan(ctx context.Context, o awsx.OrphanPS) error
}

const uninstallUsage = "uninstall --check [--region <r>] [--stack-name <name>]"

// keptOnUninstall is what the uninstall never deletes: it belongs to the organization.
const keptOnUninstall = "kept: Identity Center groups, delegated administrator"

func openUninstaller(ctx context.Context, profile, region, stack string) (Uninstaller, error) {
	return awsx.OpenUninstaller(ctx, profile, region, stack)
}

// runUninstall reads what is left of an installation; with --check it only lists it.
func runUninstall(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "uninstall")
	region := f.String("region", "", "region of the stack (default: sso_region of the profile, after confirmation)")
	stackName := f.String("stack-name", "elevate", "stack name")
	check := f.Bool("check", false, "list what is left and change nothing")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	if !*check {
		return fmt.Errorf("missing --check: %w", errUsage)
	}
	profile := f.awsProfileName(e)
	u := e.UI()
	src, err := e.Profile(ctx, profile)
	if err != nil {
		return err
	}
	// --check changes nothing: the region of the profile needs no confirmation.
	if *region, err = stackRegion(u, src, profile, *region, "Uninstall", *check); err != nil {
		return err
	}
	un, err := e.Uninstaller(ctx, profile, *region, *stackName)
	if err != nil {
		return err
	}
	where, err := un.Check(ctx)
	if err != nil {
		return err
	}
	left, err := un.Leftovers(ctx)
	if err != nil {
		return err
	}
	u.Header(fmt.Sprintf("aws-elevate uninstall · stack %s in %s · %s", *stackName, where.Account, *region))
	printLeftovers(u, *stackName, where.Account, left)
	return nil
}

// printLeftovers lists what is left, or says nothing is.
func printLeftovers(u *term.UI, stack, account string, l awsx.Leftovers) {
	w := u.Writer()
	if l.Empty() {
		u.OK("Nothing of Elevate left in "+account, "")
		fmt.Fprintf(w, "  %s\n", u.Dim(keptOnUninstall))
		return
	}
	item := func(label, detail string) { fmt.Fprintf(w, "  • %s   %s\n", label, u.Dim(detail)) }
	fmt.Fprintln(w, "  Left in "+account+":")
	if l.Stack != nil {
		item("Stack "+stack, fmt.Sprintf("%s, %d resources", l.Stack.Status, l.Stack.Resources))
	}
	if l.Table {
		item("Table "+stack+"-grants", "grant history, kept when the stack is deleted")
	}
	if l.RulesApp != "" {
		item("AppConfig application "+stack, "rules versions, kept when the stack is deleted")
	}
	if len(l.ServicePS) > 0 {
		item("Permission sets "+strings.Join(l.ServicePS, ", "), "created by the stack")
	}
	for _, o := range l.Orphans {
		where := make([]string, len(o.Assignments))
		for i, a := range o.Assignments {
			where[i] = fmt.Sprintf("%s %s in %s", strings.ToLower(a.PrincipalType), a.PrincipalID, a.Account)
		}
		detail := "not assigned"
		if len(where) > 0 {
			detail = "assigned to " + strings.Join(where, ", ")
		}
		item(o.Name, detail)
	}
	fmt.Fprintf(w, "\n  %s\n", u.Dim(keptOnUninstall))
}
