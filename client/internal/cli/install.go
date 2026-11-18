package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/release"
	"github.com/sdiaco/aws-elevate/client/internal/term"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// Installer is what the guided install calls on AWS with the admin's own profile.
type Installer interface {
	Check(ctx context.Context) (awsx.Installation, error)
	Groups(ctx context.Context, identityStoreID string) ([]awsx.Group, error)
	CreateStack(ctx context.Context, s awsx.NewStack) (string, error)
	Stack(ctx context.Context, stackID string) (awsx.StackState, error)
	TemplateResources(ctx context.Context, templateURL string) (int, error)
}

// stackPoll is how often the install reads the stack's progress.
const stackPoll = 10 * time.Second

const installUsage = "install [--region <r>] [--admin-group <name>] [--requester-group <name>] [--auditor-group <name>] [--email <address>] [--yes]"

// runInstall is the guided install: it creates the stack in the
// delegated administrator account with the admin's own profile, then runs setup.
func runInstall(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "install")
	region := f.String("region", "", "Identity Center home region (default: sso_region of the profile, after confirmation)")
	adminGroup := f.String("admin-group", "", "name of the group of admins (ElevateAdmin)")
	requesterGroup := f.String("requester-group", "", "name of the group that may request access (ElevateRequester)")
	auditorGroup := f.String("auditor-group", "", "name of the optional auditor group")
	email := f.String("email", "", "e-mail address for the alerts")
	yes := f.Bool("yes", false, "answer yes to the confirmations, for scripts")
	stackName := f.String("stack-name", "elevate", "stack name, for tests")
	templateURL := f.String("template-url", "", "template URL, for tests (default: the release bucket of the region)")
	codeBucket := f.String("code-bucket", "", "bucket of the Lambda package, for tests")
	codeKey := f.String("code-key", "", "key of the Lambda package, for tests")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	profile := f.awsProfileName(e)
	u := e.UI()
	u.Header("aws-elevate install · v" + version.Client)

	src, err := e.Profile(ctx, profile)
	if err != nil {
		return err
	}
	if *region == "" {
		// The SSO region is the Identity Center home region; another profile can only
		// suggest its own region.
		from := "sso_region of profile " + profile
		*region = src.SSORegion
		if *region == "" {
			from, *region = "region of profile "+profile, src.Region
		}
		if *region == "" {
			return errors.New("profile " + profile + " has no region: pass --region <Identity Center home region>")
		}
		ok, err := u.Confirm(fmt.Sprintf("Install in %s, the Identity Center region (%s)?", *region, from), *yes)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("install stopped: run it again with --region <Identity Center home region>")
		}
		answered(u, *yes)
	}

	// Checked even with --template-url: the template refuses an unmapped region too.
	officialURL, err := release.TemplateURL(*region, version.Client)
	if err != nil {
		return err
	}
	template := fmt.Sprintf("official v%s, bucket %s", version.Client, release.Bucket(*region))
	if *templateURL == "" {
		*templateURL = officialURL
	} else {
		template = "custom " + *templateURL
	}

	inst, err := e.Installer(ctx, profile, *region)
	if err != nil {
		return err
	}
	where, err := inst.Check(ctx)
	if err != nil {
		return err
	}
	u.OK("Account", u.Bold(where.Account)+" (Identity Center delegated administrator)")
	u.OK("Region", *region)
	u.OK("Template", template)

	stack := awsx.NewStack{Name: *stackName, TemplateURL: *templateURL, Parameters: map[string]string{}}
	if err := chooseGroups(ctx, u, inst, where, stack.Parameters, *adminGroup, *requesterGroup, *auditorGroup); err != nil {
		return err
	}
	if *email == "" {
		if *email, err = u.Ask("Alert e-mail", ""); err != nil {
			return err
		}
	}
	stack.Parameters["AlertEmail"] = *email
	if *codeBucket != "" {
		stack.Parameters["CodeBucket"] = *codeBucket
	}
	if *codeKey != "" {
		stack.Parameters["CodeKey"] = *codeKey
	}

	w := u.Writer()
	fmt.Fprintf(w, "\n  %s %s\n", u.Bold("Summary"), u.Dim(strings.Repeat("─", 39)))
	fmt.Fprintf(w, "  Stack %q in %s · %s\n", stack.Name, where.Account, *region)
	fmt.Fprintln(w, "  Creates: 3 permission sets, private API, Lambda functions, table, rules store")
	fmt.Fprintln(w, "  Idle cost: 0 $ · remove it with aws-elevate uninstall")
	ok, err := u.Confirm("Proceed?", *yes)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("install stopped: nothing was created")
	}
	fmt.Fprintln(w)

	total, err := inst.TemplateResources(ctx, *templateURL)
	if err != nil {
		return err
	}
	id, err := inst.CreateStack(ctx, stack)
	if err != nil {
		return err
	}
	p := u.Progress("Creating stack")
	if err := waitStack(ctx, e, inst, id, stack.Name, total, p); err != nil {
		return err
	}
	p.Done("Stack created", "in "+p.Elapsed().String())
	if *email != "" {
		u.Warn("Confirm the subscription in the e-mail sent to " + *email + " to receive the alerts")
	}

	if src.SSOStartURL == "" {
		return nextSetup(e, u)
	}
	if err := runSetup(ctx, e, []string{"--profile", profile, "--account", where.Account}); err != nil {
		return fmt.Errorf("%w\nThe stack is installed: run aws-elevate setup --profile %s once your access to the new permission sets shows up", err, profile)
	}
	return nil
}

// answered separates a question someone typed an answer to from the next step.
func answered(u *term.UI, yes bool) {
	if !yes {
		fmt.Fprintln(u.Writer())
	}
}

// chooseGroups fills the group parameters from the flags, else from a choice. With the
// required groups given as flags the install is unattended: an optional group not given
// as a flag means none.
func chooseGroups(ctx context.Context, u *term.UI, inst Installer, where awsx.Installation, params map[string]string, admin, requester, auditor string) error {
	groups, err := inst.Groups(ctx, where.IdentityStoreID)
	if err != nil {
		return err
	}
	slices.SortFunc(groups, func(a, b awsx.Group) int { return cmp.Compare(a.Name, b.Name) })
	items := make([]term.Item, len(groups))
	for i, g := range groups {
		items[i] = term.Item{Title: g.Name}
	}
	unattended := admin != "" && requester != ""
	for _, p := range []struct {
		param, label, name string
		optional           bool
	}{
		{"AdminGroupId", "Admin group", admin, false},
		{"RequesterGroupId", "Requester group", requester, false},
		{"AuditorGroupId", "Auditor group", auditor, true},
	} {
		switch {
		case p.name != "":
			id, err := groupID(groups, p.name)
			if err != nil {
				return err
			}
			params[p.param] = id
		case p.optional && unattended:
		default:
			i, err := u.Select(p.label, items, p.optional)
			if err != nil {
				return err
			}
			if i >= 0 {
				params[p.param] = groups[i].ID
			}
		}
	}
	return nil
}

// groupID turns a group name into its id: CloudFormation assigns permission sets by id.
func groupID(groups []awsx.Group, name string) (string, error) {
	for _, g := range groups {
		if g.Name == name {
			return g.ID, nil
		}
	}
	return "", fmt.Errorf("no group named %q in Identity Center", name)
}

// nextSetup names the SSO profile to set up when the install ran with another kind of
// profile, which cannot reach the service.
func nextSetup(e *Env, u *term.UI) error {
	names, err := awsx.SSOProfiles(e.ConfigFile)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		u.Next("aws-elevate setup --profile <a profile with sso_session>")
		return nil
	}
	u.Next("aws-elevate setup --profile " + names[0] + "   " + u.Dim("(SSO profiles found: "+strings.Join(names, ", ")+")"))
	return nil
}

func openInstaller(ctx context.Context, profile, region string) (Installer, error) {
	return awsx.OpenInstaller(ctx, profile, region)
}

// waitStack follows the creation on one progress line until it ends. On failure it shows
// the first reason, which is the precheck's message when region or account are wrong.
func waitStack(ctx context.Context, e *Env, inst Installer, id, name string, total int, p *term.Progress) error {
	seen := map[string]bool{}
	var all []awsx.StackEvent // every event, oldest first: one page may not reach the first failure
	for {
		state, err := inst.Stack(ctx, id)
		if err != nil {
			return err
		}
		for _, ev := range state.Events {
			if !seen[ev.ID] {
				seen[ev.ID] = true
				all = append(all, ev)
			}
		}
		p.Update(stackDone(all, name), total)
		switch state.Status {
		case "CREATE_COMPLETE":
			return nil
		case "DELETE_COMPLETE", "DELETE_FAILED", "ROLLBACK_COMPLETE", "ROLLBACK_FAILED":
			p.Fail("Stack not created", state.Status)
			return fmt.Errorf("stack creation failed (%s): %s", state.Status, failureReason(all))
		}
		if err := e.Sleep(ctx, stackPoll); err != nil {
			return err
		}
	}
}

// stackDone counts the resources created so far; the stack's own events are not one.
func stackDone(events []awsx.StackEvent, stack string) int {
	done := map[string]bool{}
	for _, ev := range events {
		if ev.Status == "CREATE_COMPLETE" && ev.Resource != stack {
			done[ev.Resource] = true
		}
	}
	return len(done)
}

func failureReason(events []awsx.StackEvent) string {
	for _, ev := range events {
		if strings.HasSuffix(ev.Status, "_FAILED") && ev.Reason != "Resource creation cancelled" {
			return ev.Resource + ": " + ev.Reason
		}
	}
	return "no reason in the stack events: see them in the CloudFormation console"
}
