package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// AdminAWS is what the admin commands call on AWS with ElevateAdmin credentials.
type AdminAWS interface {
	PublishRules(ctx context.Context, app, profile string, latest *int, doc []byte) (int, error)
	StackStatus(ctx context.Context, stackID string) (string, error)
	PermissionSetNames(ctx context.Context) ([]string, error)
	HasTrail(ctx context.Context) (bool, error)
	ConfirmedSubscribers(ctx context.Context, topicARN string) (int, error)
	PreviewUpdate(ctx context.Context, stackID, templateURL string) (awsx.Preview, error)
}

const adminUsage = "admin rules list | admin rules save <file> | admin export [<file>] | admin import <file> | admin doctor | admin updates [--preview <version>]"

func runAdmin(ctx context.Context, e *Env, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch {
	case sub == "rules" && len(args) > 0 && args[0] == "list":
		return runRulesList(ctx, e, args[1:])
	case sub == "rules" && len(args) > 0 && args[0] == "save":
		return runPublish(ctx, e, args[1:])
	case sub == "import":
		return runPublish(ctx, e, args)
	case sub == "export":
		return runExport(ctx, e, args)
	case sub == "doctor":
		return runDoctor(ctx, e, args)
	case sub == "updates":
		return runUpdates(ctx, e, args)
	case sub == "--help" || sub == "-h":
		fmt.Fprintf(e.Stderr, "usage: aws elevate %s\n", adminUsage)
		return nil
	}
	return fmt.Errorf("unknown admin command %q: %w", strings.TrimSpace(sub+" "+strings.Join(args, " ")), errUsage)
}

// adminSession is an admin's connection: the service, the installation it describes and
// the AWS calls made with the admin's own credentials.
type adminSession struct {
	svc  Service
	inst *api.Installation
	aws  AdminAWS
	me   *api.Me
}

func connectAdmin(ctx context.Context, e *Env, profile string) (*adminSession, error) {
	target, err := setUpTarget(e, profile)
	if err != nil {
		return nil, err
	}
	b, err := e.Open(ctx, profile)
	if err != nil {
		return nil, err
	}
	svc, err := b.Service(ctx, target)
	if err != nil {
		return nil, err
	}
	me, err := svc.Me(ctx)
	if err != nil {
		return nil, err
	}
	if me.Installation == nil {
		return nil, fmt.Errorf("admin commands need %s; profile %s signs in as %s with %s", awsx.RoleAdmin, profile, me.User.UserName, target.Role)
	}
	admin, err := b.Admin(ctx, target)
	if err != nil {
		return nil, err
	}
	return &adminSession{svc: svc, inst: me.Installation, aws: admin, me: me}, nil
}

// adminArgs parses an admin subcommand that takes at most max positional arguments.
func adminArgs(e *Env, args []string, max int) (string, []string, error) {
	f := newFlags(e, "admin")
	rest, err := f.parse(args)
	if err != nil {
		return "", nil, err
	}
	if len(rest) > max {
		return "", nil, fmt.Errorf("unexpected argument %q: %w", rest[max], errUsage)
	}
	return f.profileName(e), rest, nil
}

func runRulesList(ctx context.Context, e *Env, args []string) error {
	profile, _, err := adminArgs(e, args, 0)
	if err != nil {
		return err
	}
	s, err := connectAdmin(ctx, e, profile)
	if err != nil {
		return err
	}
	list, err := s.svc.Rules(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "Rules version %d:\n", list.ConfigVersion)
	if len(list.Rules) == 0 {
		fmt.Fprintln(e.Stdout, "No rules.")
		return nil
	}
	w := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RULE\tGROUP\tPERMISSION SET\tACCOUNTS\tDURATION\tAPPROVAL")
	for _, r := range list.Rules {
		accounts := make([]string, len(r.Accounts))
		for i, a := range r.Accounts {
			accounts[i] = a.Name + " (" + a.ID + ")"
		}
		approval := r.Approval
		if r.ApproverGroup != nil {
			approval += " by " + r.ApproverGroup.Name
		}
		if r.Privileged {
			approval += ", privileged"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s (max %s)\t%s\n", r.ID, r.Group.Name, r.PermissionSet.Name, strings.Join(accounts, ", "),
			short(r.Duration.DefaultSeconds), short(r.Duration.MaxSeconds), approval)
	}
	return w.Flush()
}

// runPublish is `admin rules save` and `admin import`: the service checks the document,
// then the admin publishes it on AppConfig from the newest version (design §7.1, §7.2).
func runPublish(ctx context.Context, e *Env, args []string) error {
	profile, rest, err := adminArgs(e, args, 1)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("missing the rules file: %w", errUsage)
	}
	doc, err := os.ReadFile(rest[0])
	if err != nil {
		return err
	}
	if !json.Valid(doc) {
		return fmt.Errorf("%s is not valid JSON", rest[0])
	}
	s, err := connectAdmin(ctx, e, profile)
	if err != nil {
		return err
	}
	check, err := s.svc.CheckRules(ctx, doc)
	if err != nil {
		return err
	}
	if !check.Valid {
		fmt.Fprintf(e.Stderr, "%s was not published:\n", rest[0])
		printCheckErrors(e, check.Errors)
		return exitCode(1)
	}
	versions, err := s.svc.ConfigVersions(ctx)
	if err != nil {
		return err
	}
	latest := newest(versions)
	n, err := s.aws.PublishRules(ctx, s.inst.RulesApplicationID, s.inst.RulesProfileID, latest, doc)
	if errors.Is(err, awsx.ErrVersionConflict) {
		return fmt.Errorf("another rules version was published after version %d: look at it with aws elevate admin export, then run the command again", *latest)
	}
	if err != nil {
		return fmt.Errorf("publish the rules on AppConfig: %w", err)
	}
	if latest != nil && n <= *latest {
		fmt.Fprintf(e.Stdout, "No change: version %d already has this content.\n", n)
		return nil
	}
	fmt.Fprintf(e.Stdout, "Published rules version %d. It applies to new requests within about 30 seconds.\n", n)
	return nil
}

// newest is the highest version number the service knows, valid or not: AppConfig
// refuses a version that does not start from the latest one.
func newest(v *api.ConfigVersions) *int {
	var n *int
	for _, info := range v.Versions {
		if n == nil || info.Number > *n {
			n = &info.Number
		}
	}
	return n
}

func printCheckErrors(e *Env, errs []api.CheckError) {
	for _, ce := range errs {
		where := "document"
		if ce.RuleID != nil {
			where = "rule " + *ce.RuleID
		}
		fmt.Fprintf(e.Stderr, "  %s: %s (%s)\n", where, ce.Message, ce.Code)
	}
}

func runExport(ctx context.Context, e *Env, args []string) error {
	profile, rest, err := adminArgs(e, args, 1)
	if err != nil {
		return err
	}
	s, err := connectAdmin(ctx, e, profile)
	if err != nil {
		return err
	}
	doc, n, err := currentDoc(ctx, s.svc)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, doc, "", "  "); err != nil {
		return fmt.Errorf("rules version %d: %w", n, err)
	}
	out.WriteByte('\n')
	if len(rest) == 0 {
		_, err := e.Stdout.Write(out.Bytes())
		return err
	}
	if err := os.WriteFile(rest[0], out.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(e.Stderr, "Wrote rules version %d to %s.\n", n, rest[0])
	return nil
}

// currentDoc is the rules document in use and its version number.
func currentDoc(ctx context.Context, svc Service) (json.RawMessage, int, error) {
	versions, err := svc.ConfigVersions(ctx)
	if err != nil {
		return nil, 0, err
	}
	if versions.Current == nil {
		return nil, 0, errors.New("no valid rules version yet: publish one with aws elevate admin rules save <file>")
	}
	v, err := svc.ConfigVersion(ctx, *versions.Current)
	if err != nil {
		return nil, 0, err
	}
	return v.Doc, *versions.Current, nil
}

// Doctor results.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "FAIL"
)

type finding struct{ status, name, text string }

// runDoctor checks the installation and says what is missing (design §10.2). Every
// check runs even when another fails; the exit status is 1 if one failed.
func runDoctor(ctx context.Context, e *Env, args []string) error {
	profile, _, err := adminArgs(e, args, 0)
	if err != nil {
		return err
	}
	s, err := connectAdmin(ctx, e, profile)
	if err != nil {
		return err
	}
	// Reaching here means the service accepted this client and the client accepted the
	// infrastructure (design §12.2).
	found := []finding{{checkOK, "versions", fmt.Sprintf("client %s, infrastructure %s (oldest client accepted %s)",
		version.Client, s.me.InfraVersion, s.me.MinClient)}}
	found = append(found, checkStack(ctx, s), checkServicePermissionSets(ctx, s))
	found = append(found, checkRules(ctx, s)...)
	found = append(found, checkTrail(ctx, s), checkAlerts(ctx, s), checkApprovals(ctx, s))

	w := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	failed := false
	for _, f := range found {
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.status, f.name, f.text)
		failed = failed || f.status == checkFail
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if failed {
		return exitCode(1)
	}
	return nil
}

func checkStack(ctx context.Context, s *adminSession) finding {
	status, err := s.aws.StackStatus(ctx, s.inst.StackID)
	switch {
	case err != nil:
		return finding{checkFail, "stack", err.Error()}
	case status == "CREATE_COMPLETE" || status == "UPDATE_COMPLETE":
		return finding{checkOK, "stack", status}
	case strings.HasSuffix(status, "_IN_PROGRESS"):
		return finding{checkWarn, "stack", status + ": an operation is running, run doctor again when it ends"}
	case status == "UPDATE_ROLLBACK_COMPLETE":
		return finding{checkWarn, "stack", status + ": the last update failed and was rolled back; see the stack events"}
	}
	return finding{checkFail, "stack", status + ": see the stack events in CloudFormation"}
}

func checkServicePermissionSets(ctx context.Context, s *adminSession) finding {
	names, err := s.aws.PermissionSetNames(ctx)
	if err != nil {
		return finding{checkFail, "permission sets", err.Error()}
	}
	var missing []string
	for _, want := range []string{awsx.RoleRequester, awsx.RoleAdmin} {
		if !slices.Contains(names, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return finding{checkFail, "permission sets", strings.Join(missing, ", ") + " missing in Identity Center: update the stack to create them again"}
	}
	return finding{checkOK, "permission sets", awsx.RoleRequester + " and " + awsx.RoleAdmin + " present"}
}

// checkRules asks the service to check the rules in use (a permission set deleted in
// AWS shows here) and reports newer versions the service ignores.
func checkRules(ctx context.Context, s *adminSession) []finding {
	versions, err := s.svc.ConfigVersions(ctx)
	if err != nil {
		return []finding{{checkFail, "rules", err.Error()}}
	}
	var out []finding
	for _, v := range versions.Versions {
		if !v.Valid && (versions.Current == nil || v.Number > *versions.Current) {
			out = append(out, finding{checkWarn, "rules", fmt.Sprintf("version %d is not valid and is ignored: %s", v.Number, strings.Join(v.Errors, "; "))})
		}
	}
	doc, n, err := currentDoc(ctx, s.svc)
	if err != nil {
		return append(out, finding{checkFail, "rules", err.Error()})
	}
	check, err := s.svc.CheckRules(ctx, doc)
	if err != nil {
		return append(out, finding{checkFail, "rules", err.Error()})
	}
	for _, ce := range check.Errors {
		where := ""
		if ce.RuleID != nil {
			where = "rule " + *ce.RuleID + ": "
		}
		out = append(out, finding{checkFail, "rules", where + ce.Message})
	}
	if check.Valid {
		out = append(out, finding{checkOK, "rules", fmt.Sprintf("version %d in use, no problem found", n)})
	}
	return out
}

func checkTrail(ctx context.Context, s *adminSession) finding {
	has, err := s.aws.HasTrail(ctx)
	switch {
	case err != nil:
		return finding{checkFail, "trail", err.Error()}
	case has:
		return finding{checkOK, "trail", "a CloudTrail trail covers this account"}
	}
	// Optional (design §7.3): elevate does not create one, a second trail costs money.
	return finding{checkWarn, "trail", "no CloudTrail trail: the free event history keeps who published rules for 90 days only"}
}

func checkAlerts(ctx context.Context, s *adminSession) finding {
	n, err := s.aws.ConfirmedSubscribers(ctx, s.inst.AlertsTopicARN)
	switch {
	case err != nil:
		return finding{checkFail, "alerts", err.Error()}
	case n == 0:
		return finding{checkFail, "alerts", "nobody receives critical alerts: confirm the e-mail sent by SNS, or subscribe to " + s.inst.AlertsTopicARN}
	}
	return finding{checkOK, "alerts", fmt.Sprintf("%d confirmed subscriber(s)", n)}
}

func checkApprovals(ctx context.Context, s *adminSession) finding {
	n, err := s.aws.ConfirmedSubscribers(ctx, s.inst.ApprovalsTopicARN)
	if err != nil {
		return finding{checkFail, "approvals", err.Error()}
	}
	// Optional (design §9.2): without subscribers, requests show in status and the UI.
	return finding{checkOK, "approvals", fmt.Sprintf("%d confirmed subscriber(s) to requests waiting for approval", n)}
}
