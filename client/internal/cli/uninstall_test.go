package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/term"
)

// fakeUninstaller answers from left, removes from it what is deleted and records every
// change in calls.
type fakeUninstaller struct {
	region        string
	stack         string
	left          awsx.Leftovers
	states        []awsx.StackState // successive answers of Stack; deleted when empty
	calls         []string
	onDeleteStack func()
}

func (f *fakeUninstaller) Check(context.Context) (awsx.Installation, error) {
	return awsx.Installation{Account: "111111111111", IdentityStoreID: "d-1"}, nil
}
func (f *fakeUninstaller) Leftovers(context.Context) (awsx.Leftovers, error) { return f.left, nil }
func (f *fakeUninstaller) DeleteStack(context.Context) error {
	f.calls = append(f.calls, "delete stack")
	if f.onDeleteStack != nil {
		f.onDeleteStack()
	}
	f.left.Stack, f.left.ServicePS = nil, nil
	return nil
}
func (f *fakeUninstaller) Stack(context.Context, string) (awsx.StackState, error) {
	if len(f.states) == 0 {
		return awsx.StackState{Status: "DELETE_COMPLETE"}, nil
	}
	s := f.states[0]
	f.states = f.states[1:]
	return s, nil
}
func (f *fakeUninstaller) DeleteTable(context.Context) error {
	f.calls = append(f.calls, "delete table")
	f.left.Table = false
	return nil
}
func (f *fakeUninstaller) DeleteRules(_ context.Context, app string) error {
	f.calls = append(f.calls, "delete rules "+app)
	f.left.RulesApp = ""
	return nil
}
func (f *fakeUninstaller) DeleteOrphan(_ context.Context, o awsx.OrphanPS) error {
	f.calls = append(f.calls, "delete "+o.Name)
	f.left.Orphans = slices.DeleteFunc(slices.Clone(f.left.Orphans), func(x awsx.OrphanPS) bool { return x.Name == o.Name })
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

// adminTarget is the ElevateAdmin profile work in the account of the stack.
var adminTarget = awsx.Target{Account: "111111111111", Role: awsx.RoleAdmin, APIURL: "https://api.example.com/v1"}

// withElevateProfiles records the profiles setup found.
func (h *harness) withElevateProfiles(t *testing.T, profiles map[string]awsx.Target) {
	t.Helper()
	if err := awsx.SaveSettings(h.env.SettingsPath, awsx.Settings{Profiles: profiles}); err != nil {
		t.Fatal(err)
	}
}

// installedEverything is a running installation with its history and no orphans.
func installedEverything() awsx.Leftovers {
	l := everythingLeft
	l.Orphans = nil
	return l
}

func uninstall(h *harness, args ...string) int {
	return h.run(append([]string{"uninstall", "--profile", "work", "--region", "eu-west-1"}, args...)...)
}

func TestUninstallStopsOnOpenGrants(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	h.svc.open = []api.Grant{grantFor("a000000001")}

	if code := uninstall(h, "--yes"); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, h.errOut)
	}
	out := h.out.String() + h.errOut.String()
	for _, want := range []string{"a000000001", `aws elevate revoke a000000001 --reason`, "1 open grant"} {
		if !strings.Contains(out, want) {
			t.Errorf("misses %q:\n%s", want, out)
		}
	}
	if len(un.calls) != 0 {
		t.Errorf("changed %v", un.calls)
	}
}

func TestUninstallPicksTheOnlyElevateAdminProfile(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	other := adminTarget
	other.Account = "222222222222"
	requester := adminTarget
	requester.Role = awsx.RoleRequester
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget, "elsewhere": other, "user": requester})

	if code := uninstall(h, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !strings.Contains(h.out.String(), "No open grants") || !strings.Contains(h.out.String(), "profile work") {
		t.Errorf("out %s", h.out)
	}
}

func TestUninstallAsksForElevateProfileWhenAmbiguous(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget, "work2": adminTarget})

	if code := uninstall(h, "--yes"); code == 0 {
		t.Fatal("exit 0, want an error")
	}
	for _, want := range []string{"work, work2", "--elevate-profile"} {
		if !strings.Contains(h.errOut.String(), want) {
			t.Errorf("stderr misses %q: %s", want, h.errOut)
		}
	}
	if len(un.calls) != 0 {
		t.Errorf("changed %v", un.calls)
	}
}

func TestUninstallKeepsHistoryByDefault(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})

	if code := uninstall(h, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !slices.Equal(un.calls, []string{"delete stack"}) {
		t.Errorf("calls %v", un.calls)
	}
	out := h.out.String()
	for _, want := range []string{"Table elevate-grants", "AppConfig application elevate", "elevate-guardrails"} {
		if !strings.Contains(out, want) {
			t.Errorf("misses %q:\n%s", want, out)
		}
	}
}

func TestUninstallExportsBeforeDeletingTheStack(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	h.svc.versions = &api.ConfigVersions{Versions: []api.VersionInfo{{Number: 1}}}
	h.svc.docs = map[int]string{1: `{"schemaVersion":1,"rules":[]}`}
	dir := filepath.Join(t.TempDir(), "history")
	exported := false
	un.onDeleteStack = func() {
		_, err := os.Stat(filepath.Join(dir, manifestFile))
		exported = err == nil
	}

	if code := uninstall(h, "--history", "export", "--export-dir", dir, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !exported {
		t.Error("the stack was deleted before the export")
	}
	if !slices.Equal(un.calls, []string{"delete stack", "delete table", "delete rules app1"}) {
		t.Errorf("calls %v", un.calls)
	}
	if !strings.Contains(h.out.String(), "Nothing of Elevate left in 111111111111") {
		t.Errorf("out %s", h.out)
	}
}

func TestUninstallNeedsTheStackNameTyped(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	// Keep the history, then a wrong name.
	h.withUninstaller(un, "1\nelevat\n")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})

	if code := uninstall(h); code == 0 {
		t.Fatal("exit 0 with the wrong name")
	}
	if len(un.calls) != 0 {
		t.Errorf("changed %v", un.calls)
	}
	if !strings.Contains(h.errOut.String(), "nothing was deleted") {
		t.Errorf("stderr %s", h.errOut)
	}
}

func TestUninstallTypedNameDeletes(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	// Delete the history, then the right name.
	h.withUninstaller(un, "3\nelevate\n")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})

	if code := uninstall(h); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !slices.Equal(un.calls, []string{"delete stack", "delete table", "delete rules app1"}) {
		t.Errorf("calls %v", un.calls)
	}
}

func TestUninstallAfterTheStackIsGoneOffersOnlyKeepOrDelete(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: awsx.Leftovers{Table: true, RulesApp: "app1"}}
	// The second choice is Delete, then the name.
	h.withUninstaller(un, "2\nelevate\n")

	if code := uninstall(h); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	out := h.out.String()
	if strings.Contains(out, "Export") {
		t.Errorf("offers an export without the stack:\n%s", out)
	}
	if !strings.Contains(out, "The stack is gone: the history can no longer be exported") {
		t.Errorf("out %s", out)
	}
	if !slices.Equal(un.calls, []string{"delete table", "delete rules app1"}) {
		t.Errorf("calls %v", un.calls)
	}
}

func TestUninstallRefusesAnExportWithoutTheStack(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: awsx.Leftovers{Table: true}}
	h.withUninstaller(un, "")

	if code := uninstall(h, "--history", "export", "--yes"); code == 0 {
		t.Fatal("exit 0")
	}
	if len(un.calls) != 0 {
		t.Errorf("changed %v", un.calls)
	}
}

func TestUninstallSkipsWhatIsAlreadyDeleted(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: awsx.Leftovers{Table: true}}
	h.withUninstaller(un, "")

	if code := uninstall(h, "--history", "delete", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !slices.Equal(un.calls, []string{"delete table"}) {
		t.Errorf("calls %v", un.calls)
	}
}

func TestUninstallFollowsAStackAlreadyBeingDeleted(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: awsx.Leftovers{Stack: &awsx.StackInfo{ID: "arn:stack/elevate/1", Status: "DELETE_IN_PROGRESS", Resources: 2}},
		states: []awsx.StackState{{Status: "DELETE_IN_PROGRESS"}}}
	h.withUninstaller(un, "")

	if code := uninstall(h, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if len(un.calls) != 0 {
		t.Errorf("deleted again: %v", un.calls)
	}
}

func TestUninstallReportsAFailedStackDeletion(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything(), states: []awsx.StackState{{Status: "DELETE_FAILED", Events: []awsx.StackEvent{
		{ID: "e1", Resource: "Api", Status: "DELETE_FAILED", Reason: "Access denied"},
	}}}}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})

	if code := uninstall(h, "--history", "delete", "--yes"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "Api: Access denied") {
		t.Errorf("stderr %s", h.errOut)
	}
	if slices.Contains(un.calls, "delete table") {
		t.Errorf("deleted the history after a failed stack deletion: %v", un.calls)
	}
}

func TestUninstallDeletesOrphansOnlyAfterConfirmation(t *testing.T) {
	h := newHarness(t)
	orphans := awsx.Leftovers{Orphans: everythingLeft.Orphans}
	un := &fakeUninstaller{left: orphans}
	h.withUninstaller(un, "n\n")
	if code := uninstall(h); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if len(un.calls) != 0 {
		t.Errorf("deleted without confirmation: %v", un.calls)
	}
	if !strings.Contains(h.out.String(), "JIT-abcde12345") {
		t.Errorf("out %s", h.out)
	}

	h = newHarness(t)
	un = &fakeUninstaller{left: orphans}
	h.withUninstaller(un, "y\n")
	if code := uninstall(h); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !slices.Equal(un.calls, []string{"delete JIT-abcde12345"}) {
		t.Errorf("calls %v", un.calls)
	}
}

func TestUninstallRemovesLocalFiles(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	cfg := "[profile work]\nregion = eu-west-1\n\n# >>> aws-elevate: managed block, rewritten automatically, do not edit >>>\n[profile jit]\n# <<< aws-elevate <<<\n"
	if err := os.WriteFile(h.env.ConfigFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := uninstall(h, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	data, err := os.ReadFile(h.env.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[profile work]\nregion = eu-west-1\n" {
		t.Errorf("config %q", data)
	}
	if _, err := os.Stat(h.env.SettingsPath); !os.IsNotExist(err) {
		t.Errorf("settings still there: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"Nothing of Elevate left in 111111111111", "not touched"} {
		if !strings.Contains(out, want) {
			t.Errorf("misses %q:\n%s", want, out)
		}
	}
}

func TestUninstallWithoutTerminalNeedsYes(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	h.env.ui = term.Plain(strings.NewReader(""), h.out, false)

	if code := uninstall(h); code == 0 {
		t.Fatal("exit 0 without --yes")
	}
	if !strings.Contains(h.errOut.String(), "--yes") {
		t.Errorf("stderr %s", h.errOut)
	}
	if len(un.calls) != 0 {
		t.Errorf("changed %v", un.calls)
	}
}
