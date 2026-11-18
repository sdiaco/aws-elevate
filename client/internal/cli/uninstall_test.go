package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

// fakeUninstaller answers from left and records every change in calls.
type fakeUninstaller struct {
	region string
	stack  string
	left   awsx.Leftovers
	states []awsx.StackState
	calls  []string
}

func (f *fakeUninstaller) Check(context.Context) (awsx.Installation, error) {
	return awsx.Installation{Account: "111111111111", IdentityStoreID: "d-1"}, nil
}
func (f *fakeUninstaller) Leftovers(context.Context) (awsx.Leftovers, error) { return f.left, nil }
func (f *fakeUninstaller) DeleteStack(context.Context) error {
	f.calls = append(f.calls, "delete stack")
	return nil
}
func (f *fakeUninstaller) Stack(context.Context, string) (awsx.StackState, error) {
	s := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return s, nil
}
func (f *fakeUninstaller) DeleteTable(context.Context) error {
	f.calls = append(f.calls, "delete table")
	return nil
}
func (f *fakeUninstaller) DeleteRules(_ context.Context, app string) error {
	f.calls = append(f.calls, "delete rules "+app)
	return nil
}
func (f *fakeUninstaller) DeleteOrphan(_ context.Context, o awsx.OrphanPS) error {
	f.calls = append(f.calls, "delete "+o.Name)
	return nil
}

// withUninstaller makes the harness uninstall with un, through the SSO profile work whose
// sso_region is eu-west-1.
func (h *harness) withUninstaller(un *fakeUninstaller, stdin string) {
	h.withInstaller(&fakeInstaller{}, stdin)
	h.env.Uninstaller = func(_ context.Context, profile, region, stack string) (Uninstaller, error) {
		if profile != "work" {
			return nil, errors.New("unexpected profile " + profile)
		}
		un.region, un.stack = region, stack
		return un, nil
	}
}

var everythingLeft = awsx.Leftovers{
	Stack:     &awsx.StackInfo{ID: "arn:stack/elevate/1", Status: "UPDATE_COMPLETE", Resources: 42},
	Table:     true,
	RulesApp:  "app1",
	ServicePS: []string{"ElevateRequester", "ElevateAdmin"},
	Orphans: []awsx.OrphanPS{{ARN: "arn:ps", Name: "JIT-abcde12345",
		Assignments: []awsx.Assignment{{Account: "222222222222", PrincipalType: "USER", PrincipalID: "u-1"}}}},
}

func TestUninstallCheckListsWhatIsLeftAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: everythingLeft}
	h.withUninstaller(un, "")
	if code := h.run("uninstall", "--check", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if un.region != "eu-west-1" || un.stack != "elevate" {
		t.Errorf("region %q stack %q", un.region, un.stack)
	}
	out := h.out.String()
	for _, want := range []string{
		"aws-elevate uninstall · stack elevate in 111111111111 · eu-west-1",
		"Stack elevate   UPDATE_COMPLETE, 42 resources",
		"Table elevate-grants",
		"AppConfig application elevate",
		"Permission sets ElevateRequester, ElevateAdmin",
		"JIT-abcde12345   assigned to user u-1 in 222222222222",
		keptOnUninstall,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("misses %q:\n%s", want, out)
		}
	}
	if len(un.calls) != 0 {
		t.Errorf("changed %v", un.calls)
	}
}

func TestUninstallCheckWhenNothingIsLeft(t *testing.T) {
	h := newHarness(t)
	h.withUninstaller(&fakeUninstaller{}, "")
	if code := h.run("uninstall", "--check", "--stack-name", "elevate-e2e", "--region", "us-east-1", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	out := h.out.String()
	if !strings.Contains(out, "✓ Nothing of Elevate left in 111111111111") || !strings.Contains(out, keptOnUninstall) {
		t.Errorf("out %q", out)
	}
	if !strings.Contains(out, "stack elevate-e2e in 111111111111 · us-east-1") {
		t.Errorf("header %q", out)
	}
}
