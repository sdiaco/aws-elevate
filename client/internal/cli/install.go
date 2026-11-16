package cli

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// Installer is what the guided install calls on AWS with the admin's own profile.
type Installer interface {
	Check(ctx context.Context) (awsx.Installation, error)
	Groups(ctx context.Context, identityStoreID string) ([]awsx.Group, error)
	CreateStack(ctx context.Context, s awsx.NewStack) (string, error)
	Stack(ctx context.Context, stackID string) (awsx.StackState, error)
}

// stackPoll is how often the install reads the stack's progress.
const stackPoll = 10 * time.Second

const installUsage = "install [--region <r>] [--admin-group <name>] [--requester-group <name>] [--auditor-group <name>] [--email <address>]"

// runInstall is the guided install of design §12.1: it creates the stack in the
// delegated administrator account with the admin's own profile, then runs setup.
func runInstall(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "install")
	region := f.String("region", "", "Identity Center home region (default: sso_region of the profile, after confirmation)")
	adminGroup := f.String("admin-group", "", "name of the group of admins (ElevateAdmin)")
	requesterGroup := f.String("requester-group", "", "name of the group that may request access (ElevateRequester)")
	auditorGroup := f.String("auditor-group", "", "name of the optional auditor group")
	email := f.String("email", "", "e-mail address for the alerts")
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
	profile := f.profileName(e)
	in := bufio.NewReader(e.Stdin)

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
		fmt.Fprintf(e.Stdout, "Identity Center region (%s): %s\n", from, *region)
		answer, err := ask(e, in, "Install there? [y/N] ")
		if err != nil {
			return err
		}
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			return errors.New("install stopped: run it again with --region <Identity Center home region>")
		}
	}

	inst, err := e.Installer(ctx, profile, *region)
	if err != nil {
		return err
	}
	where, err := inst.Check(ctx)
	if err != nil {
		return err
	}
	groups, err := inst.Groups(ctx, where.IdentityStoreID)
	if err != nil {
		return err
	}
	slices.SortFunc(groups, func(a, b awsx.Group) int { return cmp.Compare(a.Name, b.Name) })
	stack := awsx.NewStack{Name: *stackName, TemplateURL: *templateURL, Parameters: map[string]string{}}
	picks := []struct {
		param, label string
		name         *string
		optional     bool
	}{
		{"AdminGroupId", "Group of admins (ElevateAdmin)", adminGroup, false},
		{"RequesterGroupId", "Group that may request access (ElevateRequester)", requesterGroup, false},
		{"AuditorGroupId", "Auditor group, optional", auditorGroup, true},
	}
	listed := false
	for _, p := range picks {
		if *p.name == "" && !listed {
			printGroups(e.Stdout, groups)
			listed = true
		}
		id, err := pickGroup(e, in, groups, p.label, *p.name, p.optional)
		if err != nil {
			return err
		}
		if id != "" {
			stack.Parameters[p.param] = id
		}
	}
	if *email == "" {
		if *email, err = ask(e, in, "E-mail address for the alerts: "); err != nil {
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
	if stack.TemplateURL == "" {
		stack.TemplateURL = fmt.Sprintf("https://elevate-releases-%s.s3.%s.amazonaws.com/v%s/elevate.yaml", *region, *region, version.Client)
	}

	fmt.Fprintf(e.Stdout, "Creating stack %s in account %s, %s. It takes a few minutes.\n", stack.Name, where.Account, *region)
	id, err := inst.CreateStack(ctx, stack)
	if err != nil {
		return err
	}
	if err := waitStack(ctx, e, inst, id); err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "Stack created. Confirm the subscription in the e-mail sent to %s to receive the alerts.\n", *email)

	if src.SSOStartURL == "" {
		fmt.Fprintln(e.Stdout, "Next: aws-elevate setup --profile <your SSO profile>")
		return nil
	}
	if err := runSetup(ctx, e, []string{"--profile", profile, "--account", where.Account}); err != nil {
		return fmt.Errorf("%w\nThe stack is installed: run aws-elevate setup --profile %s once your access to the new permission sets shows up", err, profile)
	}
	return nil
}

func openInstaller(ctx context.Context, profile, region string) (Installer, error) {
	return awsx.OpenInstaller(ctx, profile, region)
}

// ask prints a question and reads one line. A missing answer (end of input) is empty.
func ask(e *Env, in *bufio.Reader, question string) (string, error) {
	fmt.Fprint(e.Stdout, question)
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the answer: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func printGroups(w io.Writer, groups []awsx.Group) {
	fmt.Fprintln(w, "Identity Center groups:")
	for i, g := range groups {
		fmt.Fprintf(w, "  %d) %s\n", i+1, g.Name)
	}
}

// pickGroup turns a group name, given as a flag or typed at the prompt as a name or a
// number of the list, into its id: CloudFormation assigns permission sets by id.
func pickGroup(e *Env, in *bufio.Reader, groups []awsx.Group, label, name string, optional bool) (string, error) {
	if name == "" {
		question := label + " (number or name): "
		if optional {
			question = label + " (number or name, empty for none): "
		}
		var err error
		if name, err = ask(e, in, question); err != nil {
			return "", err
		}
		if name == "" && optional {
			return "", nil
		}
		if n, err := strconv.Atoi(name); err == nil && n >= 1 && n <= len(groups) {
			return groups[n-1].ID, nil
		}
	}
	for _, g := range groups {
		if g.Name == name {
			return g.ID, nil
		}
	}
	return "", fmt.Errorf("no group named %q in Identity Center", name)
}

// waitStack prints each new stack event until the creation ends. On failure it shows the
// first reason, which is the precheck's message when region or account are wrong.
func waitStack(ctx context.Context, e *Env, inst Installer, id string) error {
	seen := map[string]bool{}
	var all []awsx.StackEvent // every event, oldest first: one page may not reach the first failure
	for {
		state, err := inst.Stack(ctx, id)
		if err != nil {
			return err
		}
		for _, ev := range state.Events {
			if seen[ev.ID] {
				continue
			}
			seen[ev.ID] = true
			all = append(all, ev)
			fmt.Fprintf(e.Stdout, "  %-40s %s\n", ev.Resource, ev.Status)
		}
		switch state.Status {
		case "CREATE_COMPLETE":
			return nil
		case "DELETE_COMPLETE", "DELETE_FAILED", "ROLLBACK_COMPLETE", "ROLLBACK_FAILED":
			return fmt.Errorf("stack creation failed (%s): %s", state.Status, failureReason(all))
		}
		if err := e.Sleep(ctx, stackPoll); err != nil {
			return err
		}
	}
}

func failureReason(events []awsx.StackEvent) string {
	for _, ev := range events {
		if strings.HasSuffix(ev.Status, "_FAILED") && ev.Reason != "Resource creation cancelled" {
			return ev.Resource + ": " + ev.Reason
		}
	}
	return "no reason in the stack events: see them in the CloudFormation console"
}
