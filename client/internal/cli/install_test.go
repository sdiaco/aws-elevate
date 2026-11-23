package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

type fakeInstaller struct {
	region   string
	checkErr error
	groups   []awsx.Group
	created  *awsx.NewStack
	states   []awsx.StackState // returned one per call, the last one repeated
	polls    int
	checked  bool
	total    int // resources in the template
}

func (f *fakeInstaller) TemplateResources(_ context.Context, url string) (int, error) {
	if url == "" {
		return 0, errors.New("no template url")
	}
	return f.total, nil
}

func (f *fakeInstaller) Check(context.Context) (awsx.Installation, error) {
	f.checked = true
	if f.checkErr != nil {
		return awsx.Installation{}, f.checkErr
	}
	return awsx.Installation{Account: "111111111111", IdentityStoreID: "d-1"}, nil
}

func (f *fakeInstaller) Groups(_ context.Context, store string) ([]awsx.Group, error) {
	if store != "d-1" {
		return nil, errors.New("wrong identity store " + store)
	}
	return f.groups, nil
}

func (f *fakeInstaller) CreateStack(_ context.Context, s awsx.NewStack) (string, error) {
	f.created = &s
	return "arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1", nil
}

func (f *fakeInstaller) Stack(_ context.Context, id string) (awsx.StackState, error) {
	if !strings.HasSuffix(id, "stack/elevate/1") {
		return awsx.StackState{}, errors.New("unexpected stack " + id)
	}
	s := f.states[min(f.polls, len(f.states)-1)]
	f.polls++
	return s, nil
}

var installGroups = []awsx.Group{{ID: "g-3", Name: "readers"}, {ID: "g-1", Name: "admins"}, {ID: "g-2", Name: "devs"}}

var installed = []awsx.StackState{
	{Status: "CREATE_IN_PROGRESS", Events: []awsx.StackEvent{{ID: "e1", Resource: "elevate", Status: "CREATE_IN_PROGRESS"}}},
	{Status: "CREATE_COMPLETE", Events: []awsx.StackEvent{
		{ID: "e1", Resource: "elevate", Status: "CREATE_IN_PROGRESS"},
		{ID: "e2", Resource: "Precheck", Status: "CREATE_COMPLETE"},
		{ID: "e3", Resource: "elevate", Status: "CREATE_COMPLETE"},
	}},
}

// withInstaller makes the harness install with inst, through the SSO profile work whose
// sso_region is eu-west-1.
func (h *harness) withInstaller(inst *fakeInstaller, stdin string) {
	h.backend.source = awsx.SourceProfile{Name: "work", SSOStartURL: "https://example.awsapps.com/start", SSORegion: "eu-west-1", Region: "eu-west-1"}
	h.env.Profile = func(_ context.Context, profile string) (awsx.SourceProfile, error) {
		if profile != "work" {
			return awsx.SourceProfile{}, errors.New("unexpected profile " + profile)
		}
		return h.backend.source, nil
	}
	h.typing(stdin)
	h.env.Installer = func(_ context.Context, profile, region string) (Installer, error) {
		if profile != "work" {
			return nil, errors.New("unexpected profile " + profile)
		}
		inst.region = region
		return inst, nil
	}
}

func TestInstallAsksAndCreatesTheStackWithGroupIds(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed, total: 2}
	// region confirmed, admin group by number, requester by name, no auditor, e-mail, no
	// organization name, proceed.
	h.withInstaller(inst, "y\n1\ndevs\n\nops@example.com\n\ny\n")

	if code := h.run("install", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if inst.region != "eu-west-1" {
		t.Errorf("region %q, want the profile sso_region", inst.region)
	}
	s := inst.created
	if s == nil {
		t.Fatal("no stack created")
	}
	wantURL := "https://aws-elevate-releases-eu-west-1.s3.eu-west-1.amazonaws.com/v" + version.Client + "/elevate.yaml"
	if s.Name != "elevate" || s.TemplateURL != wantURL {
		t.Errorf("stack %q from %q", s.Name, s.TemplateURL)
	}
	want := map[string]string{"AdminGroupId": "g-1", "RequesterGroupId": "g-2", "AlertEmail": "ops@example.com"}
	if len(s.Parameters) != len(want) {
		t.Errorf("parameters %v, want %v", s.Parameters, want)
	}
	for k, v := range want {
		if s.Parameters[k] != v {
			t.Errorf("%s = %q, want %q", k, s.Parameters[k], v)
		}
	}
	out := h.out.String()
	// groups listed by name, checks, summary, one result line, then setup.
	if !strings.Contains(out, "1) admins") || strings.Index(out, "admins") > strings.Index(out, "readers") {
		t.Errorf("groups not listed by name:\n%s", out)
	}
	for _, want := range []string{
		"aws-elevate install · v" + version.Client,
		"✓ Account   111111111111 (Identity Center delegated administrator)",
		"✓ Region   eu-west-1",
		"✓ Template   official v" + version.Client + ", bucket aws-elevate-releases-eu-west-1",
		`Stack "elevate" in 111111111111 · eu-west-1`,
		"Idle cost: 0 $ · remove it with aws-elevate uninstall",
		"✓ Stack created   in ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "ops@example.com") || !strings.Contains(out, "Signed in as") {
		t.Errorf("no subscription reminder or setup:\n%s", out)
	}
	if got, _ := setUpTarget(h.env, "work"); got != target {
		t.Errorf("setup did not run: %+v", got)
	}
}

func TestInstallWithFlagsAsksNothingAndPassesTheTestOverrides(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")

	code := h.run("install", "--profile", "work", "--region", "us-east-1",
		"--admin-group", "admins", "--requester-group", "devs", "--auditor-group", "readers",
		"--email", "ops@example.com", "--yes", "--stack-name", "elevate-e2e",
		"--template-url", "https://example.com/elevate.yaml", "--code-bucket", "b", "--code-key", "k.zip")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if inst.region != "us-east-1" {
		t.Errorf("region %q", inst.region)
	}
	s := inst.created
	if s.Name != "elevate-e2e" || s.TemplateURL != "https://example.com/elevate.yaml" {
		t.Errorf("stack %q from %q", s.Name, s.TemplateURL)
	}
	for k, v := range map[string]string{"AuditorGroupId": "g-3", "CodeBucket": "b", "CodeKey": "k.zip"} {
		if s.Parameters[k] != v {
			t.Errorf("%s = %q, want %q", k, s.Parameters[k], v)
		}
	}
}

func TestInstallWithTheGroupFlagsDoesNotAskForTheAuditor(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")

	code := h.run("install", "--profile", "work", "--region", "eu-west-1",
		"--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com", "--yes")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if out := h.out.String(); strings.Contains(out, "Auditor group") || strings.Contains(out, "Identity Center groups") {
		t.Errorf("asked for the auditor group:\n%s", out)
	}
	if _, ok := inst.created.Parameters["AuditorGroupId"]; ok {
		t.Errorf("parameters %v, want no auditor", inst.created.Parameters)
	}
}

func TestInstallStopsWhenTheRegionIsNotConfirmed(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "n\n")

	if code := h.run("install", "--profile", "work"); code == 0 {
		t.Fatal("exit 0")
	}
	if inst.created != nil || !strings.Contains(h.errOut.String(), "--region") {
		t.Errorf("created %v, stderr %s", inst.created, h.errOut)
	}
}

func TestInstallStopsOnAWrongRegionOrAccount(t *testing.T) {
	for _, msg := range []string{
		"no IAM Identity Center instance visible in eu-west-1",
		"account 111111111111 is not the Identity Center delegated administrator",
	} {
		h := newHarness(t)
		inst := &fakeInstaller{checkErr: errors.New(msg), groups: installGroups, states: installed}
		h.withInstaller(inst, "y\n")

		if code := h.run("install", "--profile", "work"); code != 1 {
			t.Errorf("exit %d", code)
		}
		if inst.created != nil || !strings.Contains(h.errOut.String(), msg) {
			t.Errorf("created %v, stderr %s", inst.created, h.errOut)
		}
	}
}

func TestInstallRefusesAnUnknownGroup(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")

	code := h.run("install", "--profile", "work", "--region", "eu-west-1",
		"--admin-group", "nobody", "--requester-group", "devs", "--email", "ops@example.com")
	if code != 1 || inst.created != nil {
		t.Fatalf("exit %d, created %v", code, inst.created)
	}
	if !strings.Contains(h.errOut.String(), `no group named "nobody"`) {
		t.Errorf("stderr %s", h.errOut)
	}
}

func TestInstallShowsWhyTheStackFailed(t *testing.T) {
	h := newHarness(t)
	reason := "Received response status [FAILED] from custom resource. Message returned: this is the management account"
	inst := &fakeInstaller{groups: installGroups, states: []awsx.StackState{
		{Status: "DELETE_COMPLETE", Events: []awsx.StackEvent{
			{ID: "e1", Resource: "Precheck", Status: "CREATE_FAILED", Reason: reason},
			{ID: "e2", Resource: "Other", Status: "CREATE_FAILED", Reason: "Resource creation cancelled"},
			{ID: "e3", Resource: "elevate", Status: "DELETE_COMPLETE"},
		}},
	}}
	h.withInstaller(inst, "")

	code := h.run("install", "--profile", "work", "--region", "eu-west-1",
		"--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com", "--yes")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	stderr := h.errOut.String()
	if !strings.Contains(stderr, "this is the management account") || strings.Contains(stderr, "cancelled") {
		t.Errorf("stderr %s", stderr)
	}
	if _, err := setUpTarget(h.env, "work"); err == nil {
		t.Error("setup ran after a failed install")
	}
}

func TestInstallSaysHowToSetUpWhenSetupFails(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	h.backend.discoverErr = errors.New("no account with ElevateAdmin or ElevateRequester")

	code := h.run("install", "--profile", "work", "--region", "eu-west-1",
		"--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com", "--yes")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "aws-elevate setup --profile work") {
		t.Errorf("stderr %s", h.errOut)
	}
}

// An admin may install with a profile that is not SSO, for example an assumed role: the
// region comes from the profile and setup is left to the user's SSO profile.
func TestInstallWithAProfileThatIsNotSSO(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "y\n\ny\n")
	h.backend.source = awsx.SourceProfile{Name: "work", Region: "us-east-1"}

	code := h.run("install", "--profile", "work", "--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if inst.region != "us-east-1" || !strings.Contains(h.out.String(), "region of profile work") {
		t.Errorf("region %q, out %s", inst.region, h.out)
	}
	if !strings.Contains(h.out.String(), "Next: aws-elevate setup --profile <a profile with sso_session>") {
		t.Errorf("out %s", h.out)
	}
	if _, err := setUpTarget(h.env, "work"); err == nil {
		t.Error("setup ran with a profile that is not SSO")
	}
}

func TestInstallRefusesARegionWithoutRelease(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")

	if code := h.run("install", "--profile", "work", "--region", "ap-south-1"); code != 1 || !strings.Contains(h.errOut.String(), "not published in ap-south-1") {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if inst.checked || inst.created != nil {
		t.Errorf("checked %v, created %v", inst.checked, inst.created)
	}
}

func TestInstallNeedsARegion(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	h.backend.source = awsx.SourceProfile{Name: "work"}

	if code := h.run("install", "--profile", "work"); code != 1 || !strings.Contains(h.errOut.String(), "--region") {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
}

func TestInstallShowsASummaryAndAsksBeforeCreating(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "n\n")

	code := h.run("install", "--profile", "work", "--region", "eu-west-1",
		"--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com")
	if code != 1 || inst.created != nil {
		t.Fatalf("exit %d, created %v", code, inst.created)
	}
	if !strings.Contains(h.out.String(), "Proceed? [y/N]") || !strings.Contains(h.errOut.String(), "install stopped") {
		t.Errorf("out %s\nstderr %s", h.out, h.errOut)
	}
}

func TestInstallWithoutTerminalNeedsYes(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	h.env.ui = nil
	h.env.Stdin = strings.NewReader("y\n")

	code := h.run("install", "--profile", "work", "--region", "eu-west-1",
		"--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com")
	if code != 1 || inst.created != nil || !strings.Contains(h.errOut.String(), "--yes") {
		t.Fatalf("exit %d, created %v, stderr %s", code, inst.created, h.errOut)
	}
}

func TestInstallWithFlagsAndYesAsksNothing(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	h.env.ui = nil // no terminal at all
	h.env.Stdin = strings.NewReader("")

	code := h.run("install", "--profile", "work", "--region", "eu-west-1",
		"--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com", "--yes")
	if code != 0 || inst.created == nil {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if strings.Contains(h.out.String(), "?") {
		t.Errorf("asked something:\n%s", h.out)
	}
}

func TestInstallProgressIsOneLine(t *testing.T) {
	h := newHarness(t)
	var events []awsx.StackEvent
	for i := range 20 {
		events = append(events, awsx.StackEvent{ID: fmt.Sprint(i), Resource: fmt.Sprintf("Resource%d", i), Status: "CREATE_COMPLETE"})
	}
	inst := &fakeInstaller{groups: installGroups, total: 20, states: []awsx.StackState{
		{Status: "CREATE_IN_PROGRESS", Events: events[:5]},
		{Status: "CREATE_IN_PROGRESS", Events: events[:15]},
		{Status: "CREATE_COMPLETE", Events: append(events, awsx.StackEvent{ID: "s", Resource: "elevate", Status: "CREATE_COMPLETE"})},
	}}
	h.withInstaller(inst, "")

	code := h.run("install", "--profile", "work", "--region", "eu-west-1",
		"--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com", "--yes")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if out := h.out.String(); strings.Contains(out, "Resource1") || !strings.Contains(out, "Stack created   in ") {
		t.Errorf("output:\n%s", out)
	}
	if done := stackDone(inst.states[2].Events, "elevate"); done != 20 {
		t.Errorf("done %d, want 20 without the stack itself", done)
	}
}

func TestInstallWithAProfileThatIsNotSSOSuggestsTheSSOProfiles(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	h.backend.source = awsx.SourceProfile{Name: "work", Region: "us-east-1"}
	config := "[profile admin]\nrole_arn = x\n[profile a]\nsso_session = s\n[profile b]\nsso_start_url = https://example.awsapps.com/start\n[sso-session s]\nsso_region = eu-west-1\n"
	if err := os.WriteFile(h.env.ConfigFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	code := h.run("install", "--profile", "work", "--region", "us-east-1", "--admin-group", "admins",
		"--requester-group", "devs", "--email", "ops@example.com", "--yes")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if out := h.out.String(); !strings.Contains(out, "Next: aws-elevate setup --profile a") || !strings.Contains(out, "(SSO profiles found: a, b)") {
		t.Errorf("out %s", out)
	}
}

func TestInstallPassesTheOrganizationName(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	code := h.run("install", "--profile", "work", "--region", "eu-west-1", "--admin-group", "admins",
		"--requester-group", "devs", "--email", "ops@example.com", "--organization-name", "Example Corp", "--yes")
	if code != 0 || inst.created.Parameters["OrganizationName"] != "Example Corp" {
		t.Fatalf("exit %d, parameters %v, stderr %s", code, inst.created, h.errOut)
	}

	// Asked when not given; empty keeps the template default.
	h = newHarness(t)
	inst = &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "\ny\n")
	code = h.run("install", "--profile", "work", "--region", "eu-west-1", "--admin-group", "admins",
		"--requester-group", "devs", "--email", "ops@example.com")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if _, ok := inst.created.Parameters["OrganizationName"]; ok || !strings.Contains(h.out.String(), "? Organization name, optional") {
		t.Errorf("parameters %v, out %s", inst.created.Parameters, h.out)
	}
}

func TestInstallPassesTheAdvancedFlags(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	code := h.run("install", "--profile", "work", "--region", "eu-west-1", "--admin-group", "admins",
		"--requester-group", "devs", "--email", "ops@example.com", "--max-open-grants", "3",
		"--event-consumer-accounts", "222222222222", "--config-versions-listed", "20", "--yes")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	want := map[string]string{"MaxOpenGrantsPerUser": "3", "EventConsumerAccounts": "222222222222", "ConfigVersionsListed": "20"}
	for k, v := range want {
		if inst.created.Parameters[k] != v {
			t.Errorf("%s = %q, want %q", k, inst.created.Parameters[k], v)
		}
	}
}

func TestInstallLeavesAdvancedParametersOutByDefault(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	code := h.run("install", "--profile", "work", "--region", "eu-west-1", "--admin-group", "admins",
		"--requester-group", "devs", "--email", "ops@example.com", "--yes")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	for _, k := range []string{"MaxOpenGrantsPerUser", "EventConsumerAccounts", "ConfigVersionsListed"} {
		if _, ok := inst.created.Parameters[k]; ok {
			t.Errorf("%s is set: %v", k, inst.created.Parameters)
		}
	}
}

func TestInstallMentionsTheAdvancedSettings(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	code := h.run("install", "--profile", "work", "--region", "eu-west-1", "--admin-group", "admins",
		"--requester-group", "devs", "--email", "ops@example.com", "--yes")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	const line = "Advanced settings: template defaults; change them with the install flags or in CloudFormation (Update, Use existing template)"
	out := h.out.String()
	i := strings.Index(out, line)
	if i < 0 {
		t.Fatalf("no advanced settings line:\n%s", out)
	}
	if n := strings.Index(out, "Next:"); n >= 0 && n < i {
		t.Errorf("line after Next:\n%s", out)
	}
}

func TestInstallSummaryListsTheAdvancedSettingsGiven(t *testing.T) {
	h := newHarness(t)
	inst := &fakeInstaller{groups: installGroups, states: installed}
	h.withInstaller(inst, "")
	code := h.run("install", "--profile", "work", "--region", "eu-west-1", "--admin-group", "admins",
		"--requester-group", "devs", "--email", "ops@example.com", "--config-versions-listed", "20",
		"--max-open-grants", "3", "--yes")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	out := h.out.String()
	if !strings.Contains(out, "\n  Advanced settings: MaxOpenGrantsPerUser=3, ConfigVersionsListed=20\n") {
		t.Errorf("no advanced settings line:\n%s", out)
	}
	if strings.Contains(out, "template defaults") {
		t.Errorf("claims template defaults:\n%s", out)
	}
}
