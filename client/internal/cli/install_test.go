package cli

import (
	"context"
	"errors"
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
}

func (f *fakeInstaller) Check(context.Context) (awsx.Installation, error) {
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
	h.backend.source = awsx.SourceProfile{Name: "work", SSORegion: "eu-west-1", Region: "eu-west-1"}
	h.env.Stdin = strings.NewReader(stdin)
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
	inst := &fakeInstaller{groups: installGroups, states: installed}
	// region confirmed, admin group by number, requester by name, no auditor, e-mail.
	h.withInstaller(inst, "y\n1\ndevs\n\nops@example.com\n")

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
	wantURL := "https://elevate-releases-eu-west-1.s3.eu-west-1.amazonaws.com/v" + version.Client + "/elevate.yaml"
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
	// groups listed by name, progress printed once per event, then setup.
	if !strings.Contains(out, "1) admins") || strings.Index(out, "admins") > strings.Index(out, "readers") {
		t.Errorf("groups not listed by name:\n%s", out)
	}
	if strings.Count(out, "Precheck") != 1 || !strings.Contains(out, "CREATE_COMPLETE") {
		t.Errorf("progress:\n%s", out)
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

	code := h.run("install", "--profile", "work", "--region", "eu-central-1",
		"--admin-group", "admins", "--requester-group", "devs", "--auditor-group", "readers",
		"--email", "ops@example.com", "--stack-name", "elevate-e2e",
		"--template-url", "https://example.com/elevate.yaml", "--code-bucket", "b", "--code-key", "k.zip")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if inst.region != "eu-central-1" {
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
		"--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com")
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
		"--admin-group", "admins", "--requester-group", "devs", "--email", "ops@example.com")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "aws-elevate setup --profile work") {
		t.Errorf("stderr %s", h.errOut)
	}
}
