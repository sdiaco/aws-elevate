package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
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
	checkErr      error // answer of Check
	leftoversErr  error // answer of Leftovers
}

func (f *fakeUninstaller) Check(context.Context) (awsx.Installation, error) {
	return awsx.Installation{Account: "111111111111", IdentityStoreID: "d-1"}, f.checkErr
}
func (f *fakeUninstaller) Leftovers(context.Context) (awsx.Leftovers, error) {
	return f.left, f.leftoversErr
}
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
		Assignments: []awsx.Assignment{{Account: "222222222222", PrincipalType: "USER", PrincipalID: "u-1", PrincipalName: "user@example.com"}}}},
}

func TestUninstallCheckListsWhatIsLeftAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: everythingLeft}
	h.withUninstaller(un, "")
	if code := h.run("uninstall", "infra", "--check", "--profile", "work"); code != 0 {
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
		"JIT-abcde12345   assigned to user user@example.com in 222222222222",
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
	if code := h.run("uninstall", "infra", "--check", "--stack-name", "elevate-e2e", "--region", "us-east-1", "--profile", "work"); code != 0 {
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
	return h.run(append([]string{"uninstall", "infra", "--profile", "work", "--region", "eu-west-1"}, args...)...)
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
	h.withUninstaller(un, "y\n1\nelevat\n")
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
	h.withUninstaller(un, "y\n3\nelevate\n")
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
	h.withUninstaller(un, "y\n2\nelevate\n")

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
	h.withUninstaller(un, "y\nn\n")
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
	h.withUninstaller(un, "y\ny\n")
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
	for _, want := range []string{"Nothing of Elevate left in 111111111111", clientStep} {
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

func TestUninstallAfterAFailedInstallDoesNotAskTheService(t *testing.T) {
	for _, status := range []string{"ROLLBACK_COMPLETE", "ROLLBACK_FAILED", "CREATE_FAILED"} {
		h := newHarness(t)
		left := installedEverything()
		left.Stack = &awsx.StackInfo{ID: "arn:stack/elevate/1", Status: status, Resources: 42}
		un := &fakeUninstaller{left: left}
		h.withUninstaller(un, "")
		h.svc.err = errors.New("the service must not be asked")

		if code := uninstall(h, "--history", "delete", "--yes"); code != 0 {
			t.Fatalf("%s: exit %d: %s", status, code, h.errOut)
		}
		if !slices.Equal(un.calls, []string{"delete stack", "delete table", "delete rules app1"}) {
			t.Errorf("%s: calls %v", status, un.calls)
		}
	}
}

func TestUninstallChecksOpenGrantsAgainBeforeDeletingTheStack(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	h.svc.openNext = []api.Grant{grantFor("a000000002")} // opened while the questions were answered

	if code := uninstall(h, "--history", "delete", "--yes"); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, h.errOut)
	}
	if len(un.calls) != 0 {
		t.Errorf("changed %v", un.calls)
	}
	if !strings.Contains(h.out.String()+h.errOut.String(), "a000000002") {
		t.Errorf("out %s", h.out)
	}
}

func TestUninstallStopsBeforeTheStackWhenTheExportFails(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	h.svc.versions = &api.ConfigVersions{Versions: []api.VersionInfo{{Number: 1}}}
	h.svc.docs = map[int]string{} // version 1 cannot be read
	dir := filepath.Join(t.TempDir(), "history")

	if code := uninstall(h, "--history", "export", "--export-dir", dir, "--yes"); code == 0 {
		t.Fatal("exit 0 with a failed export")
	}
	if len(un.calls) != 0 {
		t.Errorf("deleted after a failed export: %v", un.calls)
	}
}

func TestUninstallAsksBeforeRemovingLocalFilesWhenNothingWasFound(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{}
	h.withUninstaller(un, "y\nn\n")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})

	if code := uninstall(h, "--stack-name", "typo"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if _, err := os.Stat(h.env.SettingsPath); err != nil {
		t.Errorf("settings removed without confirmation: %v", err)
	}
	if !strings.Contains(h.out.String(), "Nothing of stack typo found in 111111111111") {
		t.Errorf("out %s", h.out)
	}
}

func TestUninstallDeletesOrphansFoundAfterTheStack(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	un.onDeleteStack = func() { un.left.Orphans = everythingLeft.Orphans }
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})

	if code := uninstall(h, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !slices.Equal(un.calls, []string{"delete stack", "delete JIT-abcde12345"}) {
		t.Errorf("calls %v", un.calls)
	}
}

func TestDeleteFailureNamesTheLastAttempt(t *testing.T) {
	events := []awsx.StackEvent{ // oldest first
		{Resource: "Api", Status: "DELETE_FAILED", Reason: "earlier attempt"},
		{Resource: "Table", Status: "DELETE_FAILED", Reason: "this attempt"},
	}
	if got := deleteFailure(events); got != "Table: this attempt" {
		t.Errorf("got %q", got)
	}
}

func TestUninstallSaysTheSecondOpenGrantsCheckIsARecheck(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})

	if code := uninstall(h, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	out := h.out.String()
	if got := strings.Count(out, "✓ No open grants"); got != 1 {
		t.Errorf("first check printed %d times: %s", got, out)
	}
	if got := strings.Count(out, "✓ Still no open grants"); got != 1 {
		t.Errorf("recheck printed %d times: %s", got, out)
	}
}

func TestUninstallAsksWhichOneWithoutAnArgument(t *testing.T) {
	h := newHarness(t)
	h.withUninstaller(&fakeUninstaller{}, "2\nn\n")
	h.withClientFiles(t)
	if code := h.run("uninstall"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	for _, want := range []string{"Elevate from AWS, for the whole organization (admins only)", "aws-elevate from this computer"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("misses %q:\n%s", want, h.out)
		}
	}
	if !strings.Contains(h.out.String(), "Remove aws-elevate from this computer?") {
		t.Errorf("the client was not chosen:\n%s", h.out)
	}
}

func TestUninstallNeedsAnArgumentWithYes(t *testing.T) {
	h := newHarness(t)
	h.withUninstaller(&fakeUninstaller{}, "")
	if code := h.run("uninstall", "--yes"); code != 2 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !strings.Contains(h.errOut.String(), "uninstall --yes needs infra or client") {
		t.Errorf("stderr %s", h.errOut)
	}
}

func TestUninstallCheckAsksWhichOneToo(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{}
	h.withUninstaller(un, "1\n")
	if code := h.run("uninstall", "--check", "--profile", "work", "--region", "eu-west-1"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !strings.Contains(h.out.String(), "Nothing of Elevate left in 111111111111") {
		t.Errorf("out %s", h.out)
	}
}

func TestUninstallInfraStartsWithAConfirmation(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "y\nn\n") // the region, then the organization
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})

	if code := h.run("uninstall", "infra", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	out := h.out.String()
	region := strings.Index(out, "Uninstall in eu-west-1")
	org := strings.Index(out, "Uninstall Elevate from AWS, for the whole organization? [y/N]")
	if region < 0 || org < region {
		t.Errorf("the organization is not asked right after the region:\n%s", out)
	}
	if !strings.Contains(h.errOut.String(), "uninstall stopped: nothing was deleted") {
		t.Errorf("stderr %s", h.errOut)
	}
	if len(un.calls) != 0 || strings.Contains(out, "To delete") {
		t.Errorf("went on: %v\n%s", un.calls, out)
	}
}

func TestUninstallInfraStopsBeforeDeletingWithoutAdminRights(t *testing.T) {
	denied := &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized"}
	for name, un := range map[string]*fakeUninstaller{
		"check":     {left: installedEverything(), checkErr: fmt.Errorf("list: %w", denied)},
		"leftovers": {left: installedEverything(), leftoversErr: fmt.Errorf("read stack: %w", denied)},
	} {
		h := newHarness(t)
		h.withUninstaller(un, "")
		h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
		cfg := "# >>> aws-elevate: managed block, rewritten automatically, do not edit >>>\n[profile jit]\n# <<< aws-elevate <<<\n"
		if err := os.WriteFile(h.env.ConfigFile, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}

		if code := uninstall(h, "--history", "delete", "--yes"); code != 1 {
			t.Fatalf("%s: exit %d: %s", name, code, h.errOut)
		}
		if !strings.Contains(h.errOut.String(), "you need the account administrator role of 111111111111 to uninstall Elevate") {
			t.Errorf("%s: stderr %s", name, h.errOut)
		}
		if len(un.calls) != 0 {
			t.Errorf("%s: changed %v", name, un.calls)
		}
		if data, err := os.ReadFile(h.env.ConfigFile); err != nil || string(data) != cfg {
			t.Errorf("%s: config touched: %q %v", name, data, err)
		}
		if _, err := os.Stat(h.env.SettingsPath); err != nil {
			t.Errorf("%s: settings removed: %v", name, err)
		}
	}
}

func TestUninstallInfraKeepsOtherErrorsAsTheyAre(t *testing.T) {
	h := newHarness(t)
	h.withUninstaller(&fakeUninstaller{checkErr: errors.New("network down")}, "")
	if code := uninstall(h, "--yes"); code != 1 || !strings.Contains(h.errOut.String(), "network down") {
		t.Errorf("exit %d: %s", code, h.errOut)
	}
}

func TestUninstallInfraHighlightsTheStackName(t *testing.T) {
	h := newHarness(t)
	un := &fakeUninstaller{left: installedEverything()}
	h.withUninstaller(un, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	ui := term.Colored(strings.NewReader("y\n1\nelevate\n"), h.out, true)
	h.env.ui = ui

	if code := uninstall(h); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	out := h.out.String()
	for _, want := range []string{"Type " + ui.Bold("elevate") + " to delete it", "To delete: stack " + ui.Bold("elevate")} {
		if !strings.Contains(out, want) {
			t.Errorf("misses %q:\n%s", want, out)
		}
	}
}

func TestUninstallInfraLinksTheStackConsole(t *testing.T) {
	h := newHarness(t)
	h.withUninstaller(&fakeUninstaller{left: installedEverything()}, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	if code := uninstall(h, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	want := "Follow it in the console (account 111111111111): " + stackConsoleURL("eu-west-1", "arn:stack/elevate/1")
	if !strings.Contains(h.out.String(), want) {
		t.Errorf("misses %q:\n%s", want, h.out)
	}
}

func TestUninstallInfraSCPStepIsNotAFix(t *testing.T) {
	h := newHarness(t)
	h.withUninstaller(&fakeUninstaller{left: installedEverything()}, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	if code := uninstall(h, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	out := h.out.String()
	want := "SCP stack, if you installed it (management account):\n    aws cloudformation delete-stack --stack-name elevate-guardrails --profile <management admin>"
	if !strings.Contains(out, want) {
		t.Errorf("misses %q:\n%s", want, out)
	}
	if strings.Contains(out, "Fix:") {
		t.Errorf("the step is not a fix:\n%s", out)
	}
}

func TestUninstallInfraEndsWithTheClientStep(t *testing.T) {
	h := newHarness(t)
	h.withUninstaller(&fakeUninstaller{left: installedEverything()}, "")
	h.withElevateProfiles(t, map[string]awsx.Target{"work": adminTarget})
	if code := uninstall(h, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if got := strings.TrimSpace(lines[len(lines)-1]); got != clientStep {
		t.Errorf("last line %q", got)
	}
	if strings.Contains(h.out.String(), "not touched") {
		t.Errorf("out %s", h.out)
	}
}

func TestLeftoversNameThePrincipals(t *testing.T) {
	o := awsx.OrphanPS{Assignments: []awsx.Assignment{
		{Account: "111111111111", PrincipalType: "USER", PrincipalID: "u-1", PrincipalName: "user@example.com"},
		{Account: "111111111111", PrincipalType: "GROUP", PrincipalID: "g-1", PrincipalName: "platform"},
		{Account: "222222222222", PrincipalType: "USER", PrincipalID: "u-2"},
	}}
	want := "assigned to user user@example.com in 111111111111, group platform in 111111111111, user u-2 in 222222222222"
	if got := assignedTo(o); got != want {
		t.Errorf("got %q", got)
	}
}
