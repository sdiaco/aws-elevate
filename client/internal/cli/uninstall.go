package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
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

const uninstallUsage = "uninstall [infra | client] [--check] [--yes] …\n" +
	"  infra:  [--elevate-profile <p>] [--region <r>] [--stack-name <name>]\n" +
	"          [--history keep|export|delete] [--export-dir <dir>]"

// keptOnUninstall is what the uninstall never deletes: it belongs to the organization.
const keptOnUninstall = "kept: Identity Center groups, delegated administrator"

// clientStep is what is left to do on this computer when the installation is gone.
const clientStep = "To remove the client from this computer: aws-elevate uninstall client"

// errUninstallClientPending stands for the removal of the client until it exists.
var errUninstallClientPending = errors.New("uninstall client: not available yet")

const (
	historyKeep   = "keep"
	historyExport = "export"
	historyDelete = "delete"
)

func openUninstaller(ctx context.Context, profile, region, stack string) (Uninstaller, error) {
	return awsx.OpenUninstaller(ctx, profile, region, stack)
}

// runUninstall removes the installation of the organization (infra) or the client of this
// computer (client); without either it asks which.
func runUninstall(ctx context.Context, e *Env, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "infra":
			return runUninstallInfra(ctx, e, args[1:])
		case "client":
			return runUninstallClient(ctx, e, args[1:])
		}
	}
	if slices.Contains(args, "--yes") || slices.Contains(args, "-yes") {
		return fmt.Errorf("uninstall --yes needs infra or client: %w", errUsage)
	}
	items := []term.Item{
		{Title: "infra", Detail: "Elevate from AWS, for the whole organization (admins only)"},
		{Title: "client", Detail: "aws-elevate from this computer"},
	}
	i, err := e.UI().Select("Uninstall", items, false)
	if err != nil {
		return err
	}
	answered(e.UI(), false)
	if i == 0 {
		return runUninstallInfra(ctx, e, args)
	}
	return runUninstallClient(ctx, e, args)
}

func runUninstallClient(context.Context, *Env, []string) error {
	return errUninstallClientPending
}

// uninstallRun is one uninstall: every step reads what is left and skips what is done.
type uninstallRun struct {
	e       *Env
	u       *term.UI
	un      Uninstaller
	stack   string
	account string
	region  string
	yes     bool
}

// needAdminRights turns an access denied answer into what to do about it.
func needAdminRights(err error, account string) error {
	if awsx.IsAccessDenied(err) {
		return fmt.Errorf("you need the account administrator role of %s to uninstall Elevate", account)
	}
	return err
}

// runUninstallInfra lists what is left of an installation and, without --check, removes it.
func runUninstallInfra(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "uninstall infra")
	elevateProfile := f.String("elevate-profile", "", "profile set up with ElevateAdmin in the account of the stack (default: the only one)")
	region := f.String("region", "", "region of the stack (default: sso_region of the profile, after confirmation)")
	stackName := f.String("stack-name", "elevate", "stack name")
	check := f.Bool("check", false, "list what is left and change nothing")
	history := f.String("history", "", "keep, export or delete the grant history (default: ask; keep with --yes)")
	exportDir := f.String("export-dir", "", "folder for the exported history (default: ask)")
	yes := f.Bool("yes", false, "confirm every question without asking")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	if !slices.Contains([]string{"", historyKeep, historyExport, historyDelete}, *history) {
		return fmt.Errorf("--history %q: want keep, export or delete: %w", *history, errUsage)
	}
	profile := f.awsProfileName(e)
	u := e.UI()
	src, err := e.Profile(ctx, profile)
	if err != nil {
		return err
	}
	// --check changes nothing: the region of the profile needs no confirmation.
	if *region, err = stackRegion(u, src, profile, *region, "Uninstall", *check || *yes); err != nil {
		return err
	}
	if !*check {
		ok, err := u.Confirm("Uninstall Elevate from AWS, for the whole organization?", *yes)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("uninstall stopped: nothing was deleted")
		}
		answered(u, *yes)
	}
	un, err := e.Uninstaller(ctx, profile, *region, *stackName)
	if err != nil {
		return err
	}
	// Reading what is left is also the check of the rights: it stops here, before any
	// deletion, remote or local.
	where, err := un.Check(ctx)
	if err != nil {
		return needAdminRights(err, where.Account)
	}
	left, err := un.Leftovers(ctx)
	if err != nil {
		return needAdminRights(err, where.Account)
	}
	u.Header(fmt.Sprintf("aws-elevate uninstall · stack %s in %s · %s", *stackName, where.Account, *region))
	if *check {
		printLeftovers(u, *stackName, where.Account, left)
		return nil
	}
	r := &uninstallRun{e: e, u: u, un: un, stack: *stackName, account: where.Account, region: *region, yes: *yes}
	return r.run(ctx, left, *elevateProfile, *history, *exportDir)
}

func (r *uninstallRun) run(ctx context.Context, left awsx.Leftovers, elevateProfile, history, exportDir string) error {
	live := left.Stack != nil && serviceRuns(left.Stack.Status)
	var svc Service
	if live {
		var err error
		if svc, err = r.openGrantsCheck(ctx, elevateProfile, "No open grants"); err != nil {
			return err
		}
	}
	hasHistory := left.Table || left.RulesApp != ""
	if hasHistory {
		var err error
		if history, err = r.chooseHistory(history, live); err != nil {
			return err
		}
	}
	deleteHistory := hasHistory && (history == historyExport || history == historyDelete)
	if left.Stack != nil || deleteHistory {
		if err := r.confirmDeletion(left.Stack != nil, deleteHistory); err != nil {
			return err
		}
	}
	if hasHistory && history == historyExport {
		if exportDir == "" {
			var err error
			def := "./elevate-history-" + time.Now().Format("2006-01-02")
			if exportDir, err = r.u.Ask("Export to", def); err != nil {
				return err
			}
		}
		if err := exportStep(ctx, r.e, svc, exportDir, time.Now()); err != nil {
			return err
		}
	}
	if left.Stack != nil {
		if live {
			if _, err := r.openGrantsCheck(ctx, elevateProfile, "Still no open grants"); err != nil {
				return err
			}
		}
		if err := r.deleteStack(ctx, left.Stack); err != nil {
			return err
		}
	}
	if deleteHistory {
		if err := r.deleteHistory(ctx, left); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.u.Writer(), "  SCP stack, if you installed it (management account):\n    %s\n\n",
		"aws cloudformation delete-stack --stack-name elevate-guardrails --profile <management admin>")
	// A grant opened while the questions were answered leaves its permission set behind.
	now, err := r.un.Leftovers(ctx)
	if err != nil {
		return err
	}
	if err := r.deleteOrphans(ctx, now.Orphans); err != nil {
		return err
	}
	if left.Empty() {
		ok, err := r.u.Confirm(fmt.Sprintf("Nothing of stack %s found in %s: remove the local grant profiles and settings anyway?", r.stack, r.account), r.yes)
		if err != nil {
			return err
		}
		answered(r.u, r.yes)
		if !ok {
			r.u.Warn("Local files kept: check --stack-name, --region and --profile")
			return nil
		}
	}
	if err := r.removeLocalFiles(); err != nil {
		return err
	}
	final, err := r.un.Leftovers(ctx)
	if err != nil {
		return err
	}
	printLeftovers(r.u, r.stack, r.account, final)
	fmt.Fprintf(r.u.Writer(), "  %s\n", clientStep)
	return nil
}

// openGrantsCheck connects with the ElevateAdmin profile and stops while a grant is open:
// deleting the stack would leave its access without an end. done is the line printed
// when none is open.
func (r *uninstallRun) openGrantsCheck(ctx context.Context, profile, done string) (Service, error) {
	profile, err := r.elevateProfile(profile)
	if err != nil {
		return nil, err
	}
	_, svc, err := connect(ctx, r.e, profile)
	if err != nil {
		return nil, err
	}
	open, err := listAll(ctx, svc, api.ListQuery{Scope: "all", Open: true})
	if err != nil {
		return nil, fmt.Errorf("read the open grants: %w", err)
	}
	if len(open) == 0 {
		r.u.OK(done, "read with profile "+profile)
		return svc, nil
	}
	w := r.u.Writer()
	r.u.Fail(fmt.Sprintf("%d open grant(s): revoke them first", len(open)), "")
	for _, g := range open {
		fmt.Fprintf(w, "    %s  %s  %s  %s\n", g.ID, g.Requester.UserName, g.Account, g.State)
		fmt.Fprintf(w, "      %s\n", r.u.Dim(fmt.Sprintf("aws elevate revoke %s --reason \"uninstall\" --profile %s", g.ID, profile)))
	}
	return nil, exitCode(1)
}

// elevateProfile is the flag, or the only profile set up with ElevateAdmin in the account.
func (r *uninstallRun) elevateProfile(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	settings, err := awsx.LoadSettings(r.e.SettingsPath)
	if err != nil {
		return "", err
	}
	var found []string
	for _, name := range slices.Sorted(maps.Keys(settings.Profiles)) {
		t := settings.Profiles[name]
		if t.Role == awsx.RoleAdmin && t.Account == r.account {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no profile set up with %s in %s: run aws-elevate setup --profile <p> --account %s, then pass --elevate-profile <p>",
			awsx.RoleAdmin, r.account, r.account)
	}
	return "", fmt.Errorf("profiles set up with %s in %s: %s; choose one with --elevate-profile",
		awsx.RoleAdmin, r.account, strings.Join(found, ", "))
}

// chooseHistory is the flag, the choice, or keep when nobody can answer.
func (r *uninstallRun) chooseHistory(flag string, live bool) (string, error) {
	if !live {
		r.u.Warn("The stack is gone: the history can no longer be exported")
		if flag == historyExport {
			return "", errors.New("--history export needs the stack: if an earlier run exported it, check the folder and rerun with --history delete; otherwise use --history keep")
		}
	}
	if flag != "" {
		return flag, nil
	}
	if r.yes || !r.u.Interactive() {
		return historyKeep, nil
	}
	choices := []string{historyKeep}
	items := []term.Item{{Title: "Keep", Detail: "the table and the rules stay in the account"}}
	if live {
		choices = append(choices, historyExport)
		items = append(items, term.Item{Title: "Export then delete", Detail: "to a local folder, verified before deleting"})
	}
	choices = append(choices, historyDelete)
	items = append(items, term.Item{Title: "Delete", Detail: "grants and rules versions are lost"})
	i, err := r.u.Select("History", items, false)
	if err != nil {
		return "", err
	}
	answered(r.u, false)
	return choices[i], nil
}

// confirmDeletion lists what goes and asks for the stack name typed back.
func (r *uninstallRun) confirmDeletion(stack, history bool) error {
	var what []string
	if stack {
		what = append(what, "stack "+r.u.Bold(r.stack))
	}
	if history {
		what = append(what, "grant history and rules versions")
	}
	fmt.Fprintf(r.u.Writer(), "  To delete: %s\n", strings.Join(what, ", "))
	if r.yes {
		return nil
	}
	if !r.u.Interactive() {
		return term.ErrNeedsYes
	}
	name, err := r.u.Ask("Type "+r.u.Bold(r.stack)+" to delete it", "")
	if err != nil {
		return err
	}
	if name != r.stack {
		return errors.New("uninstall stopped: the name does not match, nothing was deleted")
	}
	answered(r.u, false)
	return nil
}

// deleteStack starts the deletion unless it is running, then follows it to the end.
func (r *uninstallRun) deleteStack(ctx context.Context, info *awsx.StackInfo) error {
	if info.Status != "DELETE_IN_PROGRESS" {
		if err := r.un.DeleteStack(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.u.Writer(), "  Follow it in the console (account %s): %s\n", r.account, stackConsoleURL(r.region, info.ID))
	p := r.u.Progress("Deleting stack")
	for {
		state, err := r.un.Stack(ctx, info.ID)
		if err != nil {
			return err
		}
		p.Update(stackDeleted(state.Events, r.stack), info.Resources)
		switch state.Status {
		case "DELETE_COMPLETE":
			p.Done("Stack deleted", "in "+p.Elapsed().String())
			return nil
		case "DELETE_FAILED":
			p.Fail("Stack not deleted", state.Status)
			return fmt.Errorf("stack deletion failed: %s; fix it and run uninstall again", deleteFailure(state.Events))
		}
		if err := r.e.Sleep(ctx, stackPoll); err != nil {
			return err
		}
	}
}

// serviceRuns says whether the stack serves requests: not while it is deleted, and not
// after a failed install, which rolled the API back.
func serviceRuns(status string) bool {
	switch status {
	case "CREATE_FAILED", "ROLLBACK_IN_PROGRESS", "ROLLBACK_COMPLETE", "ROLLBACK_FAILED":
		return false
	}
	return !strings.HasPrefix(status, "DELETE_")
}

// stackDeleted counts the resources gone so far; retained ones are skipped, not deleted.
func stackDeleted(events []awsx.StackEvent, stack string) int {
	done := map[string]bool{}
	for _, ev := range events {
		if (ev.Status == "DELETE_COMPLETE" || ev.Status == "DELETE_SKIPPED") && ev.Resource != stack {
			done[ev.Resource] = true
		}
	}
	return len(done)
}

func deleteFailure(events []awsx.StackEvent) string {
	for _, ev := range slices.Backward(events) { // oldest first: the last failure is this attempt's
		if ev.Status == "DELETE_FAILED" && ev.Reason != "" {
			return ev.Resource + ": " + ev.Reason
		}
	}
	return "no reason in the stack events: see them in the CloudFormation console"
}

func (r *uninstallRun) deleteHistory(ctx context.Context, left awsx.Leftovers) error {
	if left.Table {
		if err := r.un.DeleteTable(ctx); err != nil {
			return err
		}
		r.u.OK("Table deleted", r.stack+"-grants")
	}
	if left.RulesApp != "" {
		if err := r.un.DeleteRules(ctx, left.RulesApp); err != nil {
			return err
		}
		r.u.OK("Rules deleted", "AppConfig application "+r.stack)
	}
	return nil
}

// deleteOrphans removes the JIT- permission sets left by a failed engine step, after an
// explicit confirmation: assignments first, then the permission set.
func (r *uninstallRun) deleteOrphans(ctx context.Context, orphans []awsx.OrphanPS) error {
	if len(orphans) == 0 {
		return nil
	}
	w := r.u.Writer()
	fmt.Fprintln(w, "  Leftover grant permission sets:")
	for _, o := range orphans {
		fmt.Fprintf(w, "    • %s   %s\n", o.Name, r.u.Dim(assignedTo(o)))
	}
	ok, err := r.u.Confirm(fmt.Sprintf("Delete these %d permission sets and their assignments?", len(orphans)), r.yes)
	if err != nil {
		return err
	}
	answered(r.u, r.yes)
	if !ok {
		return nil
	}
	for _, o := range orphans {
		if err := r.un.DeleteOrphan(ctx, o); err != nil {
			return err
		}
		r.u.OK("Deleted "+o.Name, "")
	}
	return nil
}

// removeLocalFiles takes the grant profiles out of the AWS CLI config and deletes the
// settings of this client.
func (r *uninstallRun) removeLocalFiles() error {
	removed, err := awsx.RemoveManagedBlock(r.e.ConfigFile)
	if err != nil {
		return err
	}
	if removed {
		r.u.OK("Grant profiles removed", r.e.ConfigFile)
	}
	if removed, err = awsx.RemoveSettings(r.e.SettingsPath); err != nil {
		return err
	}
	if removed {
		r.u.OK("Settings removed", r.e.SettingsPath)
	}
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
		item(o.Name, assignedTo(o))
	}
	fmt.Fprintf(w, "\n  %s\n", u.Dim(keptOnUninstall))
}

func assignedTo(o awsx.OrphanPS) string {
	if len(o.Assignments) == 0 {
		return "not assigned"
	}
	where := make([]string, len(o.Assignments))
	for i, a := range o.Assignments {
		who := a.PrincipalName
		if who == "" {
			who = a.PrincipalID
		}
		where[i] = fmt.Sprintf("%s %s in %s", strings.ToLower(a.PrincipalType), who, a.Account)
	}
	return "assigned to " + strings.Join(where, ", ")
}
