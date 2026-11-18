package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

func (f *fakeService) CheckRules(_ context.Context, doc []byte) (*api.CheckResult, error) {
	f.checked = append(f.checked, string(doc))
	if f.check == nil {
		return &api.CheckResult{Valid: true}, nil
	}
	return f.check, nil
}

func (f *fakeService) ConfigVersions(context.Context) (*api.ConfigVersions, error) {
	if f.versions == nil {
		return &api.ConfigVersions{}, nil
	}
	return f.versions, nil
}

func (f *fakeService) ConfigVersion(_ context.Context, n int) (*api.ConfigVersion, error) {
	doc, ok := f.docs[n]
	if !ok {
		return nil, &api.Error{Status: 404, Code: "not_found", Message: "no rules version"}
	}
	return &api.ConfigVersion{Version: api.VersionInfo{Number: n, Valid: true}, Doc: json.RawMessage(doc)}, nil
}

type publishCall struct {
	app, profile string
	latest       *int
	doc          string
}

// fakeAdmin answers the admin AWS calls; the zero value is a healthy installation
// except for what each test sets.
type fakeAdmin struct {
	accounts       []awsx.Named
	published      []publishCall
	publishN       int
	publishErr     error
	stack          string
	stackErr       error
	psNames        []string
	psErr          error
	noTrail        bool
	trailErr       error
	subs           map[string]int
	subsErr        error
	preview        awsx.Preview
	previewed      []string // stack id and template url
	permissionSets []awsx.Named
	psReads        int
}

func (a *fakeAdmin) PreviewUpdate(_ context.Context, stackID, templateURL string) (awsx.Preview, error) {
	a.previewed = []string{stackID, templateURL}
	return a.preview, nil
}

func (a *fakeAdmin) PublishRules(_ context.Context, app, profile string, latest *int, doc []byte) (int, error) {
	a.published = append(a.published, publishCall{app, profile, latest, string(doc)})
	return a.publishN, a.publishErr
}

func (a *fakeAdmin) StackStatus(context.Context, string) (string, error) { return a.stack, a.stackErr }

func (a *fakeAdmin) PermissionSets(context.Context) ([]awsx.Named, error) {
	a.psReads++
	return a.permissionSets, nil
}

func (a *fakeAdmin) Accounts(context.Context) ([]awsx.Named, error) { return a.accounts, nil }

func (a *fakeAdmin) Groups(context.Context) ([]awsx.Named, error) { return nil, nil }

func (a *fakeAdmin) PermissionSetNames(context.Context) ([]string, error) { return a.psNames, a.psErr }

func (a *fakeAdmin) HasTrail(context.Context) (bool, error) { return !a.noTrail, a.trailErr }

func (a *fakeAdmin) ConfirmedSubscribers(_ context.Context, topic string) (int, error) {
	return a.subs[topic], a.subsErr
}

var installation = &api.Installation{
	StackID:            "arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1",
	RulesApplicationID: "app1",
	RulesProfileID:     "prof1",
	AlertsTopicARN:     "arn:aws:sns:eu-west-1:111111111111:elevate-alerts",
	ApprovalsTopicARN:  "arn:aws:sns:eu-west-1:111111111111:elevate-approvals",
}

const rulesDoc = `{"schemaVersion":1,"rules":[]}`

// adminHarness is a set-up admin profile with a healthy installation, rules version 3
// in use and version 4 published but invalid.
func adminHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.setUp(t)
	h.svc.installation = installation
	h.svc.versions = &api.ConfigVersions{Current: ptr(3), Versions: []api.VersionInfo{
		{Number: 4, Valid: false, Errors: []string{"schema: approval must be auto or manual"}},
		{Number: 3, Valid: true},
	}}
	h.svc.docs = map[int]string{3: rulesDoc}
	h.backend.admin = &fakeAdmin{
		publishN: 5,
		stack:    "UPDATE_COMPLETE",
		psNames:  []string{"AdministratorAccess", awsx.RoleRequester, awsx.RoleAdmin},
		subs:     map[string]int{installation.AlertsTopicARN: 1},
	}
	return h
}

func rulesFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAdminCommandsNeedElevateAdmin(t *testing.T) {
	h := adminHarness(t)
	h.svc.installation = nil
	if code := h.run("admin", "doctor", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "admin commands need ElevateAdmin") {
		t.Fatalf("stderr: %s", h.errOut)
	}
}

func TestSavePublishesFromTheNewestVersionEvenIfInvalid(t *testing.T) {
	h := adminHarness(t)
	file := rulesFile(t, rulesDoc)
	if code := h.run("admin", "rules", "save", file, "--profile", "work"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if len(h.svc.checked) != 1 || h.svc.checked[0] != rulesDoc {
		t.Fatalf("checked %v", h.svc.checked)
	}
	got := h.backend.admin.published
	if len(got) != 1 || got[0].app != "app1" || got[0].profile != "prof1" || got[0].doc != rulesDoc || got[0].latest == nil || *got[0].latest != 4 {
		t.Fatalf("published %+v", got)
	}
	if !strings.Contains(h.out.String(), "Published rules version 5") {
		t.Fatalf("stdout: %s", h.out)
	}
}

func TestSaveRefusedByTheCheckPublishesNothing(t *testing.T) {
	h := adminHarness(t)
	h.svc.check = &api.CheckResult{Valid: false, Errors: []api.CheckError{
		{RuleID: ptr("db-admin-prod"), Code: "ps_not_found", Message: "permission set db-admin does not exist"},
		{Code: "invalid_document", Message: "schemaVersion must be 1"},
	}}
	if code := h.run("admin", "rules", "save", rulesFile(t, rulesDoc), "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if len(h.backend.admin.published) != 0 {
		t.Fatal("published a refused document")
	}
	for _, want := range []string{"was not published", "rule db-admin-prod: permission set db-admin does not exist (ps_not_found)", "document: schemaVersion must be 1"} {
		if !strings.Contains(h.errOut.String(), want) {
			t.Fatalf("stderr misses %q: %s", want, h.errOut)
		}
	}
}

func TestSaveRefusedForAStaleVersion(t *testing.T) {
	h := adminHarness(t)
	h.backend.admin.publishErr = awsx.ErrVersionConflict
	if code := h.run("admin", "rules", "save", rulesFile(t, rulesDoc), "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "published after version 4") || !strings.Contains(h.errOut.String(), "run the command again") {
		t.Fatalf("stderr: %s", h.errOut)
	}
}

func TestSaveOtherPublishErrorIsReported(t *testing.T) {
	h := adminHarness(t)
	h.backend.admin.publishErr = errors.New("AccessDeniedException")
	if code := h.run("admin", "rules", "save", rulesFile(t, rulesDoc), "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "publish the rules on AppConfig: AccessDeniedException") {
		t.Fatalf("stderr: %s", h.errOut)
	}
}

func TestSaveTheFirstVersionHasNoStartingVersion(t *testing.T) {
	h := adminHarness(t)
	h.svc.versions = &api.ConfigVersions{}
	h.backend.admin.publishN = 1
	if code := h.run("admin", "import", rulesFile(t, rulesDoc), "--profile", "work"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if got := h.backend.admin.published; len(got) != 1 || got[0].latest != nil {
		t.Fatalf("published %+v", got)
	}
}

func TestSaveSameContentSaysNoChange(t *testing.T) {
	h := adminHarness(t)
	h.backend.admin.publishN = 4
	if code := h.run("admin", "rules", "save", rulesFile(t, rulesDoc), "--profile", "work"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "No change: version 4") {
		t.Fatalf("stdout: %s", h.out)
	}
}

func TestSaveNeedsAReadableJSONFile(t *testing.T) {
	for name, args := range map[string][]string{
		"missing argument": {"admin", "rules", "save"},
		"not JSON":         {"admin", "rules", "save", rulesFile(t, "{rules")},
		"no file":          {"admin", "import", filepath.Join(t.TempDir(), "nope.json")},
	} {
		t.Run(name, func(t *testing.T) {
			h := adminHarness(t)
			if code := h.run(append(args, "--profile", "work")...); code == 0 {
				t.Fatal("exit 0")
			}
			if len(h.backend.admin.published) != 0 || len(h.svc.checked) != 0 {
				t.Fatal("called the service")
			}
		})
	}
}

func TestExportWritesTheRulesInUse(t *testing.T) {
	h := adminHarness(t)
	if code := h.run("admin", "export", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	want := "{\n  \"schemaVersion\": 1,\n  \"rules\": []\n}\n"
	if h.out.String() != want {
		t.Fatalf("stdout %q", h.out)
	}
	file := filepath.Join(t.TempDir(), "backup.json")
	if code := h.run("admin", "export", file, "--profile", "work"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got, _ := os.ReadFile(file); string(got) != want || !strings.Contains(h.errOut.String(), "Wrote rules version 3") {
		t.Fatalf("file %q, stderr %s", got, h.errOut)
	}
}

func TestExportWithoutAValidVersion(t *testing.T) {
	h := adminHarness(t)
	h.svc.versions = &api.ConfigVersions{}
	if code := h.run("admin", "export", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "no valid rules version yet") {
		t.Fatalf("stderr: %s", h.errOut)
	}
}

func TestRulesListShowsEveryRuleWithGroups(t *testing.T) {
	h := adminHarness(t)
	r := rule
	r.Group = api.Ref{ID: "g-req", Name: "backend-devs"}
	r.ApproverGroup = &api.Ref{ID: "g-lead", Name: "leads"}
	h.svc.rules = []api.Rule{r}
	if code := h.run("admin", "rules", "list", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	line := lineWith(h.out.String(), "ssm-prod")
	for _, want := range []string{"backend-devs", "ssm-write", "prod (222222222222)", "1h (max 1h30m)", "manual by leads, privileged"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line misses %q: %s", want, line)
		}
	}
}

func TestDoctorHealthyInstallation(t *testing.T) {
	h := adminHarness(t)
	h.svc.versions.Versions = h.svc.versions.Versions[1:]
	if code := h.run("admin", "doctor", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out, h.errOut)
	}
	if strings.Contains(h.out.String(), "FAIL") || strings.Contains(h.out.String(), "warn") {
		t.Fatalf("stdout: %s", h.out)
	}
	for _, name := range []string{"versions", "stack", "permission sets", "rules", "trail", "alerts", "approvals"} {
		if lineWith(squash(h.out.String()), "ok "+name) == "" {
			t.Fatalf("no ok line for %s: %s", name, h.out)
		}
	}
}

func TestDoctorReportsEveryFailingCheck(t *testing.T) {
	h := adminHarness(t)
	h.svc.check = &api.CheckResult{Valid: false, Errors: []api.CheckError{
		{RuleID: ptr("ssm-prod"), Code: "ps_not_found", Message: "permission set ssm-write does not exist in Identity Center"},
	}}
	a := h.backend.admin
	a.stack = "ROLLBACK_COMPLETE"
	a.psNames = []string{awsx.RoleAdmin}
	a.noTrail = true
	a.subs = nil
	if code := h.run("admin", "doctor", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	out := squash(h.out.String())
	for _, want := range []string{
		"FAIL stack ROLLBACK_COMPLETE",
		"FAIL permission sets ElevateRequester missing",
		"warn rules version 4 is not valid and is ignored: schema: approval must be auto or manual",
		"FAIL rules rule ssm-prod: permission set ssm-write does not exist",
		"warn trail no CloudTrail trail",
		"FAIL alerts nobody receives critical alerts",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout misses %q:\n%s", want, out)
		}
	}
}

func TestDoctorReportsAWSErrorsAndKeepsChecking(t *testing.T) {
	h := adminHarness(t)
	a := h.backend.admin
	denied := errors.New("AccessDeniedException")
	a.stackErr, a.psErr, a.trailErr, a.subsErr = denied, denied, denied, denied
	h.svc.versions = &api.ConfigVersions{}
	if code := h.run("admin", "doctor", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	out := squash(h.out.String())
	for _, name := range []string{"stack", "permission sets", "trail", "alerts", "approvals"} {
		if !strings.Contains(lineWith(out, "FAIL "+name), "AccessDeniedException") {
			t.Fatalf("%s line:\n%s", name, out)
		}
	}
	if !strings.Contains(lineWith(out, "FAIL rules"), "no valid rules version yet") {
		t.Fatalf("rules line:\n%s", out)
	}
}

func TestDoctorSaysWhenTheAccountIsNoLongerTheDelegatedAdministrator(t *testing.T) {
	h := adminHarness(t)
	h.backend.admin.psErr = fmt.Errorf("list: %w", awsx.ErrNoInstance)
	if code := h.run("admin", "doctor", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	line := lineWith(squash(h.out.String()), "FAIL identity center")
	for _, want := range []string{
		"account 111111111111 is no longer the Identity Center delegated administrator",
		"aws organizations register-delegated-administrator --account-id 111111111111 --service-principal sso.amazonaws.com",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("line misses %q:\n%s", want, h.out)
		}
	}
}

func TestDoctorStackInProgressOrRolledBackIsAWarning(t *testing.T) {
	for _, status := range []string{"UPDATE_IN_PROGRESS", "UPDATE_ROLLBACK_COMPLETE"} {
		h := adminHarness(t)
		h.svc.versions.Versions = h.svc.versions.Versions[1:]
		h.backend.admin.stack = status
		if code := h.run("admin", "doctor", "--profile", "work"); code != 0 {
			t.Fatalf("%s: exit %d", status, code)
		}
		if !strings.HasPrefix(lineWith(h.out.String(), "stack"), "warn") {
			t.Fatalf("%s: %s", status, h.out)
		}
	}
}

func TestUnknownAdminCommandIsAUsageError(t *testing.T) {
	h := adminHarness(t)
	for _, args := range [][]string{{"admin"}, {"admin", "rules"}, {"admin", "nope"}} {
		if code := h.run(args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
	if code := h.run("admin", "--help"); code != 0 {
		t.Fatalf("help: exit %d", code)
	}
}

// squash makes the doctor table independent of the column widths.
func squash(s string) string { return regexp.MustCompile(` {2,}`).ReplaceAllString(s, " ") }
