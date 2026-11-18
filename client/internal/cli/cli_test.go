package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// fakeService answers like the api; err, when set, is the answer to every call.
type fakeService struct {
	err      error
	rules    []api.Rule
	created  *api.CreateRequest
	states   []string // successive states returned by Grant
	grant    api.Grant
	lists    map[string][]api.Grant
	calls    []string
	revokeBy map[string]string
	roles    []string // roles of GET /me; requester when empty
	// admin side
	installation *api.Installation
	check        *api.CheckResult
	checked      []string
	versions     *api.ConfigVersions
	docs         map[int]string
}

func (f *fakeService) Me(context.Context) (*api.Me, error) {
	if f.err != nil {
		return nil, f.err
	}
	roles := f.roles
	if roles == nil {
		roles = []string{"requester"}
	}
	return &api.Me{User: api.User{UserName: "user@example.com"}, Roles: roles, InfraVersion: "0.0.1", MinClient: "0.0.1", Installation: f.installation}, nil
}

func (f *fakeService) Rules(context.Context) (*api.RuleList, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &api.RuleList{Rules: f.rules}, nil
}

// RequestableRules hides the admin-only rules, as the service does for scope=requestable.
func (f *fakeService) RequestableRules(ctx context.Context) (*api.RuleList, error) {
	list, err := f.Rules(ctx)
	if err != nil {
		return nil, err
	}
	var mine []api.Rule
	for _, r := range list.Rules {
		if !strings.HasPrefix(r.ID, "admin-only") {
			mine = append(mine, r)
		}
	}
	return &api.RuleList{Rules: mine}, nil
}

func (f *fakeService) PermissionSetPermissions(ctx context.Context, arn string) (*api.Permissions, error) {
	return f.Permissions(ctx, arn)
}

func (f *fakeService) Permissions(context.Context, string) (*api.Permissions, error) {
	return &api.Permissions{}, f.err
}

func (f *fakeService) CreateGrant(_ context.Context, req api.CreateRequest) (*api.Grant, error) {
	f.created = &req
	if f.err != nil {
		return nil, f.err
	}
	g := f.grant
	g.State = f.states[0]
	f.states = f.states[1:]
	return &g, nil
}

func (f *fakeService) Grant(_ context.Context, id string) (*api.GrantDetail, error) {
	f.calls = append(f.calls, "get "+id)
	if f.err != nil {
		return nil, f.err
	}
	g := f.grant
	g.ID = id
	if len(f.states) > 0 {
		g.State = f.states[0]
		f.states = f.states[1:]
	}
	return &api.GrantDetail{Grant: g}, nil
}

func (f *fakeService) Grants(_ context.Context, q api.ListQuery) (*api.GrantList, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &api.GrantList{Items: f.lists[q.Scope]}, nil
}

func (f *fakeService) action(name, id string) (*api.Grant, error) {
	f.calls = append(f.calls, name+" "+id)
	if f.err != nil {
		return nil, f.err
	}
	reason := map[string]string{"approve": "", "reject": "rejected", "cancel": "cancelled", "revoke": "revoked"}[name]
	g := api.Grant{ID: id, State: api.StateClosed, CloseReason: &reason}
	if name == "approve" {
		g.State = api.StateActivating
	}
	return &g, nil
}

func (f *fakeService) Approve(_ context.Context, id string) (*api.Grant, error) {
	return f.action("approve", id)
}
func (f *fakeService) Reject(_ context.Context, id, reason string) (*api.Grant, error) {
	f.calls = append(f.calls, "reason "+reason)
	return f.action("reject", id)
}
func (f *fakeService) Cancel(_ context.Context, id string) (*api.Grant, error) {
	return f.action("cancel", id)
}
func (f *fakeService) Revoke(_ context.Context, id, reason string) (*api.Grant, error) {
	if f.revokeBy == nil {
		f.revokeBy = map[string]string{}
	}
	f.revokeBy[id] = reason
	return f.action("revoke", id)
}

func (f *fakeService) Redrive(_ context.Context, id string) (*api.Grant, error) {
	return f.action("redrive", id)
}

type fakeBackend struct {
	svc         *fakeService
	target      awsx.Target
	discover    string // account passed to Discover
	readyRole   string
	readyErr    error
	admin       *fakeAdmin
	source      awsx.SourceProfile // zero: the default below
	discoverErr error
}

func (b *fakeBackend) Source() awsx.SourceProfile {
	if b.source != (awsx.SourceProfile{}) {
		return b.source
	}
	return awsx.SourceProfile{Name: "work", SSOSession: "corp", Region: "eu-west-1"}
}

func (b *fakeBackend) Discover(_ context.Context, account string) (awsx.Target, error) {
	b.discover = account
	return b.target, b.discoverErr
}

func (b *fakeBackend) WaitReady(_ context.Context, account, role string) error {
	b.readyRole = account + "/" + role
	return b.readyErr
}

func (b *fakeBackend) Service(context.Context, awsx.Target) (Service, error) { return b.svc, nil }

func (b *fakeBackend) Admin(context.Context, awsx.Target) (AdminAWS, error) { return b.admin, nil }

type harness struct {
	env     *Env
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	backend *fakeBackend
	svc     *fakeService
	sleeps  []time.Duration
	shell   []string
	vars    map[string]string
}

var target = awsx.Target{Account: "111111111111", Role: awsx.RoleRequester, APIURL: "https://api.example.com/v1"}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, svc: &fakeService{}, vars: map[string]string{}}
	h.backend = &fakeBackend{svc: h.svc, target: target}
	h.env = &Env{
		Stdout:  h.out,
		Stderr:  h.errOut,
		Getenv:  func(k string) string { return h.vars[k] },
		Environ: func() []string { return []string{"HOME=/home/u", "AWS_PROFILE=old", "AWS_ACCESS_KEY_ID=AKID"} },
		Open: func(_ context.Context, profile string) (Backend, error) {
			if profile != "work" {
				return nil, errors.New("unexpected profile " + profile)
			}
			return h.backend, nil
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			return nil
		},
		Shell: func(shell string, env []string) (int, error) {
			h.shell = append([]string{shell}, env...)
			return 0, nil
		},
		SettingsPath: filepath.Join(dir, "settings.json"),
		ConfigFile:   filepath.Join(dir, "config"),
		AliasFile:    filepath.Join(dir, "alias"),
		LookPath:     func(string) (string, error) { return "/usr/local/bin/aws-elevate", nil },
	}
	return h
}

// setUp records the service for profile work, as setup does.
func (h *harness) setUp(t *testing.T) {
	t.Helper()
	if err := awsx.SaveSettings(h.env.SettingsPath, awsx.Settings{Profiles: map[string]awsx.Target{"work": target}}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) run(args ...string) int {
	return Run(context.Background(), h.env, args)
}

func ptr[T any](v T) *T { return &v }

var rule = api.Rule{
	ID:            "ssm-prod",
	PermissionSet: api.PermissionSet{Name: "ssm-write"},
	Accounts:      []api.Ref{{ID: "222222222222", Name: "prod"}},
	Duration:      api.Duration{DefaultSeconds: 3600, MaxSeconds: 5400},
	Approval:      "manual",
	Privileged:    true,
}

func grantFor(id string) api.Grant {
	expires := time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)
	return api.Grant{
		ID: id, RuleID: "ssm-prod", Account: "222222222222", SourcePs: api.PermissionSet{Name: "ssm-write"},
		Requester: api.User{UserName: "user@example.com"}, ApproverGroup: &api.Ref{ID: "g-1", Name: "leads"},
		ExpiresAt: &expires,
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]int{"45m": 2700, "2h": 7200, "1h30m": 5400, "": 0} {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("%q: got %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"45", "two hours", "-1h", "0m", "1.5s"} {
		if _, err := parseDuration(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestShortDuration(t *testing.T) {
	for in, want := range map[int]string{3600: "1h", 2700: "45m", 5400: "1h30m", 30: "30s", 43200: "12h"} {
		if got := short(in); got != want {
			t.Errorf("%d: got %q, want %q", in, got, want)
		}
	}
}

func TestEveryAPIErrorExitsNonZeroWithItsMessage(t *testing.T) {
	for _, code := range api.ErrorCodes {
		t.Run(code, func(t *testing.T) {
			h := newHarness(t)
			h.setUp(t)
			h.vars["AWS_PROFILE"] = "work"
			h.svc.err = &api.Error{Status: 400, Code: code, Message: "the service says no"}
			if got := h.run("status"); got != 1 {
				t.Errorf("exit %d", got)
			}
			want := code
			if code == api.CodeClientTooOld {
				want = "aws-elevate update"
			}
			if msg := h.errOut.String(); !strings.Contains(msg, want) || !strings.Contains(msg, "the service says no") {
				t.Errorf("message %q", msg)
			}
		})
	}
}

func TestInfraTooOldExitsNonZero(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.err = api.ErrInfraTooOld
	if got := h.run("status", "--profile", "work"); got != 1 || !strings.Contains(h.errOut.String(), "infrastructure") {
		t.Errorf("exit %d, %q", got, h.errOut.String())
	}
}

func TestSetupRemembersTheServiceAndAddsTheAlias(t *testing.T) {
	h := newHarness(t)
	if got := h.run("setup", "--profile", "work", "--account", "shared"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	if h.backend.discover != "shared" {
		t.Errorf("discover with %q", h.backend.discover)
	}
	s, err := awsx.LoadSettings(h.env.SettingsPath)
	if err != nil || s.Profiles["work"] != target {
		t.Errorf("settings %+v, %v", s, err)
	}
	alias, _ := os.ReadFile(h.env.AliasFile)
	if string(alias) != "[toplevel]\n"+awsx.AliasLine+"\n" {
		t.Errorf("alias file %q", alias)
	}
	if !strings.Contains(h.out.String(), "user@example.com") {
		t.Errorf("output %q", h.out.String())
	}
}

func TestSetupSuggestsWhatTheRoleCanDo(t *testing.T) {
	for roles, want := range map[string]string{
		"requester":          "Next: aws elevate request",
		"approver,requester": "Next: aws elevate request",
		"auditor":            "Next: aws elevate ui",
		"approver":           "Next: aws elevate ui",
	} {
		h := newHarness(t)
		h.svc.roles = strings.Split(roles, ",")
		if got := h.run("setup", "--profile", "work"); got != 0 {
			t.Fatalf("exit %d: %s", got, h.errOut.String())
		}
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("%s: output %q, want %q", roles, h.out.String(), want)
		}
	}
}

func TestSetupUsesAWSProfileThenDefault(t *testing.T) {
	h := newHarness(t)
	h.vars["AWS_PROFILE"] = "work"
	if got := h.run("setup"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	h = newHarness(t)
	if got := h.run("setup"); got != 1 || !strings.Contains(h.errOut.String(), "unexpected profile default") {
		t.Errorf("exit %d, %q", got, h.errOut.String())
	}
}

func TestCommandsBeforeSetupSayWhatToRun(t *testing.T) {
	h := newHarness(t)
	if got := h.run("status", "--profile", "work"); got != 1 || !strings.Contains(h.errOut.String(), "aws-elevate setup --profile work") {
		t.Errorf("exit %d, %q", got, h.errOut.String())
	}
}

func TestRequestWithoutArgumentsListsTheRules(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	adminOnly := rule
	adminOnly.ID = "admin-only-rule"
	h.svc.rules = []api.Rule{rule, adminOnly}
	if got := h.run("request", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	out := h.out.String()
	for _, want := range []string{"ssm-prod", "ssm-write", "prod (222222222222)", "1h (max 1h30m)", "manual, privileged"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "admin-only-rule") {
		t.Error("listed a rule the caller cannot request")
	}
	if h.svc.created != nil {
		t.Error("created a grant")
	}
}

func TestRequestWithNoRules(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	if got := h.run("request", "--profile", "work"); got != 0 || !strings.Contains(h.out.String(), "No rule") {
		t.Errorf("exit %d, %q", got, h.out.String())
	}
}

func TestRequestWaitsWritesTheProfileAndPrintsIt(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.rules = []api.Rule{rule}
	h.svc.grant = grantFor("abcde12345")
	h.svc.states = []string{api.StateActivating, api.StateActivating, api.StateActivating, api.StateActive}

	got := h.run("request", "ssm-prod", "--account", "prod", "--duration", "45m", "--reason", "incident 42", "--profile", "work")
	if got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	want := api.CreateRequest{RuleID: "ssm-prod", Account: "222222222222", DurationSeconds: 2700, Reason: "incident 42"}
	if *h.svc.created != want {
		t.Errorf("created %+v", *h.svc.created)
	}
	if len(h.sleeps) != 3 || h.sleeps[0] != 2*time.Second || h.sleeps[2] != 8*time.Second {
		t.Errorf("sleeps %v", h.sleeps)
	}
	if h.backend.readyRole != "222222222222/JIT-abcde12345" {
		t.Errorf("waited for %q", h.backend.readyRole)
	}
	// The same --profile the interface shows, so one form is learned everywhere.
	if h.out.String() != "--profile jit-ssm-write-222222222222\n" {
		t.Errorf("stdout %q", h.out.String())
	}
	cfg, _ := os.ReadFile(h.env.ConfigFile)
	if !strings.Contains(string(cfg), "sso_role_name = JIT-abcde12345") || !strings.Contains(string(cfg), "sso_session = corp") {
		t.Errorf("profile %q", cfg)
	}
}

func TestRequestKeepsAnUnknownAccountAsTyped(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.rules = []api.Rule{rule}
	h.svc.grant = grantFor("abcde12345")
	h.svc.states = []string{api.StateActive}
	if got := h.run("request", "ssm-prod", "--account", "333333333333", "--reason", "x", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	if h.svc.created.Account != "333333333333" || h.svc.created.DurationSeconds != 0 {
		t.Errorf("created %+v", *h.svc.created)
	}
}

func TestRequestPendingExitsAndSaysWhenItExpires(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.grant = grantFor("abcde12345")
	h.svc.grant.ApprovalExpiresAt = ptr(time.Now().Add(29*time.Minute + 50*time.Second))
	h.svc.states = []string{api.StatePending}
	got := h.run("request", "ssm-prod", "--account", "prod", "--reason", "x", "--profile", "work")
	if got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	msg := h.errOut.String()
	for _, want := range []string{"abcde12345", "group leads", "expires in 30 min", "aws elevate shell abcde12345"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
	if len(h.sleeps) != 0 || h.backend.readyRole != "" || h.out.Len() != 0 {
		t.Errorf("waited: sleeps %v, ready %q, stdout %q", h.sleeps, h.backend.readyRole, h.out.String())
	}
}

func TestRequestClosedWhileActivating(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.grant = grantFor("abcde12345")
	h.svc.grant.CloseReason = ptr("failed")
	h.svc.grant.Error = ptr("CreatePermissionSet: AccessDenied")
	h.svc.states = []string{api.StateActivating, api.StateClosed}
	got := h.run("request", "ssm-prod", "--account", "prod", "--reason", "x", "--profile", "work")
	if got != 1 || !strings.Contains(h.errOut.String(), "closed: failed (CreatePermissionSet: AccessDenied)") {
		t.Errorf("exit %d, %q", got, h.errOut.String())
	}
	if h.backend.readyRole != "" {
		t.Error("waited for a closed grant")
	}
}

func TestRequestStopsWhenInterrupted(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.grant = grantFor("abcde12345")
	h.svc.states = []string{api.StateActivating}
	h.env.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	got := h.run("request", "ssm-prod", "--account", "prod", "--reason", "x", "--profile", "work")
	if got != 1 || !strings.Contains(h.errOut.String(), "stopped waiting for abcde12345") {
		t.Errorf("exit %d, %q", got, h.errOut.String())
	}
}

func TestRequestNotReady(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.grant = grantFor("abcde12345")
	h.svc.states = []string{api.StateActive}
	h.backend.readyErr = errors.New("role JIT-abcde12345 not usable after 56s")
	got := h.run("request", "ssm-prod", "--account", "prod", "--reason", "x", "--profile", "work")
	if got != 1 || !strings.Contains(h.errOut.String(), "not usable") || h.out.Len() != 0 {
		t.Errorf("exit %d, %q, stdout %q", got, h.errOut.String(), h.out.String())
	}
}

func TestStatusShowsOwnGrantsAndOnlyApprovableRequests(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	own := grantFor("mine000001")
	own.State = api.StateActive
	own.Privileged = true
	toApprove := grantFor("other00001")
	toApprove.State = api.StatePending
	toApprove.Actions = []string{"view", "approve", "reject"}
	ownPending := grantFor("mine000002")
	ownPending.State = api.StatePending
	ownPending.Actions = []string{"view", "cancel"}
	ownPending.ExpiresAt = nil
	ownPending.ApprovalExpiresAt = ptr(time.Date(2026, 11, 15, 9, 30, 0, 0, time.Local))
	h.svc.lists = map[string][]api.Grant{"mine": {own, ownPending}, "approvable": {toApprove, ownPending}}

	if got := h.run("status", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	out := h.out.String()
	_, approveSection, ok := strings.Cut(out, "To approve")
	if !ok || !strings.Contains(approveSection, "other00001") || strings.Contains(approveSection, "mine000002") {
		t.Errorf("output %q", out)
	}
	if row := lineWith(out, "mine000002"); !strings.Contains(row, "09:30") {
		t.Errorf("pending row without its approval deadline: %q", out)
	}
	if !strings.Contains(out, "mine000001") || !strings.Contains(out, "privileged") {
		t.Errorf("output %q", out)
	}
}

func TestStatusEmpty(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	if got := h.run("status", "--profile", "work"); got != 0 || !strings.Contains(h.out.String(), "no open grant") ||
		strings.Contains(h.out.String(), "To approve") {
		t.Errorf("exit %d, %q", got, h.out.String())
	}
}

func TestRevokeCancelsOrRevokesAsTheServiceOffers(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.grant = grantFor("x")
	h.svc.grant.Actions = []string{"view", "cancel"}
	if got := h.run("revoke", "pend000001", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	h.svc.grant.Actions = []string{"view", "revoke"}
	if got := h.run("revoke", "actv000001", "actv000002", "--reason", "done", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	want := "get pend000001,cancel pend000001,get actv000001,revoke actv000001,get actv000002,revoke actv000002"
	if strings.Join(h.svc.calls, ",") != want {
		t.Errorf("calls %v", h.svc.calls)
	}
	if h.svc.revokeBy["actv000002"] != "done" {
		t.Errorf("reason %q", h.svc.revokeBy["actv000002"])
	}
	if !strings.Contains(h.out.String(), "actv000001: closed, revoked") {
		t.Errorf("output %q", h.out.String())
	}
}

// failOn fails the calls of one grant id and answers the others.
type failOn struct {
	*fakeService
	id string
}

func (f failOn) Grant(ctx context.Context, id string) (*api.GrantDetail, error) {
	if id == f.id {
		return nil, &api.Error{Status: 404, Code: api.CodeNotFound, Message: "no such grant"}
	}
	return f.fakeService.Grant(ctx, id)
}

func TestRevokeReportsEachGrantAndFailsIfOneFails(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.grant = grantFor("x")
	h.svc.grant.Actions = []string{"revoke"}
	h.env.Open = func(context.Context, string) (Backend, error) {
		return &svcBackend{h.backend, failOn{h.svc, "gone000001"}}, nil
	}
	got := h.run("revoke", "gone000001", "actv000001", "--reason", "done", "--profile", "work")
	if got != 1 {
		t.Errorf("exit %d", got)
	}
	if !strings.Contains(h.errOut.String(), "gone000001: not_found") || !strings.Contains(h.errOut.String(), "1 of 2 not done") ||
		!strings.Contains(h.out.String(), "actv000001: closed, revoked") {
		t.Errorf("stderr %q, stdout %q", h.errOut.String(), h.out.String())
	}
}

type svcBackend struct {
	*fakeBackend
	svc Service
}

func (b *svcBackend) Service(context.Context, awsx.Target) (Service, error) { return b.svc, nil }

func TestRevokeWithoutIDPicksTheOnlyOpenGrant(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	one := grantFor("only000001")
	one.State = api.StateActive
	one.Actions = []string{"view", "revoke"}
	closedView := grantFor("view000001")
	closedView.Actions = []string{"view"}
	h.svc.lists = map[string][]api.Grant{"mine": {one, closedView}}
	h.svc.grant = one
	if got := h.run("revoke", "--reason", "done", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	if h.svc.revokeBy["only000001"] != "done" {
		t.Errorf("calls %v", h.svc.calls)
	}

	two := one
	two.ID = "only000002"
	h.svc.lists["mine"] = []api.Grant{one, two}
	if got := h.run("revoke", "--reason", "done", "--profile", "work"); got != 1 ||
		!strings.Contains(h.errOut.String(), "only000001 (ssm-write on 222222222222), only000002") {
		t.Errorf("exit %d, %q", got, h.errOut.String())
	}

	h.svc.lists["mine"] = nil
	if got := h.run("revoke", "--profile", "work"); got != 1 || !strings.Contains(h.errOut.String(), "no active grant") {
		t.Errorf("exit %d, %q", got, h.errOut.String())
	}
}

func TestApproveAndReject(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	if got := h.run("approve", "pend000001", "--profile", "work"); got != 0 || !strings.Contains(h.out.String(), "pend000001: activating") {
		t.Fatalf("exit %d: %s %s", got, h.errOut.String(), h.out.String())
	}
	if got := h.run("approve", "--reject", "pend000002", "--profile", "work"); got != 2 || !strings.Contains(h.errOut.String(), "--reason") {
		t.Fatalf("rejected without a reason: exit %d, %q", got, h.errOut.String())
	}
	if got := h.run("approve", "--reason", "not now", "pend000002", "--profile", "work"); got != 2 || !strings.Contains(h.errOut.String(), "--reject") {
		t.Fatalf("reason without --reject: exit %d, %q", got, h.errOut.String())
	}
	if got := h.run("approve", "--reject", "--reason", "use the read-only rule", "pend000002", "--profile", "work"); got != 0 || !strings.Contains(h.out.String(), "pend000002: closed, rejected") {
		t.Fatalf("exit %d: %s %s", got, h.errOut.String(), h.out.String())
	}
	if strings.Join(h.svc.calls, ",") != "approve pend000001,reason use the read-only rule,reject pend000002" {
		t.Errorf("calls %v", h.svc.calls)
	}
	if got := h.run("approve", "--profile", "work"); got != 2 || !strings.Contains(h.errOut.String(), "usage: aws elevate approve") {
		t.Errorf("exit %d, %q", got, h.errOut.String())
	}
}

func TestShellStartsWithTheGrantProfile(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.vars["SHELL"] = "/bin/zsh"
	active := grantFor("actv000001")
	active.State = api.StateActive
	h.svc.lists = map[string][]api.Grant{"mine": {active}}
	if got := h.run("shell", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	if h.backend.readyRole != "222222222222/JIT-actv000001" {
		t.Errorf("did not wait for the role: %q", h.backend.readyRole)
	}
	want := "/bin/zsh,HOME=/home/u,AWS_PROFILE=jit-ssm-write-222222222222"
	if strings.Join(h.shell, ",") != want {
		t.Errorf("shell %v", h.shell)
	}
	cfg, _ := os.ReadFile(h.env.ConfigFile)
	if !strings.Contains(string(cfg), "sso_role_name = JIT-actv000001") {
		t.Errorf("profile %q", cfg)
	}
}

func TestShellByIDAndItsExitCode(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.grant = grantFor("x")
	h.svc.states = []string{api.StateActive, api.StatePending}
	h.env.Shell = func(shell string, _ []string) (int, error) {
		if shell != "/bin/sh" {
			t.Errorf("shell %q", shell)
		}
		return 3, nil
	}
	if got := h.run("shell", "actv000001", "--profile", "work"); got != 3 {
		t.Errorf("exit %d: %s", got, h.errOut.String())
	}
	if got := h.run("shell", "pend000001", "--profile", "work"); got != 1 || !strings.Contains(h.errOut.String(), "pending, not active") {
		t.Errorf("exit %d, %q", got, h.errOut.String())
	}
}

func TestHelpVersionAndUsage(t *testing.T) {
	h := newHarness(t)
	if got := h.run(); got != 0 || !strings.Contains(h.out.String(), "request") || !strings.Contains(h.out.String(), "approve") {
		t.Errorf("help: exit %d, %q", got, h.out.String())
	}
	h = newHarness(t)
	if got := h.run("--version"); got != 0 || h.out.String() != "aws-elevate "+version.Client+"\n" {
		t.Errorf("version: exit %d, %q", got, h.out.String())
	}
	h = newHarness(t)
	if got := h.run("request", "--help"); got != 0 || !strings.Contains(h.errOut.String(), "--duration") {
		t.Errorf("command help: exit %d, %q", got, h.errOut.String())
	}
	h = newHarness(t)
	if got := h.run("frobnicate"); got != 2 || !strings.Contains(h.errOut.String(), `unknown command "frobnicate"`) {
		t.Errorf("unknown: exit %d, %q", got, h.errOut.String())
	}
	h = newHarness(t)
	if got := h.run("status", "--bogus"); got != 2 || !strings.Contains(h.errOut.String(), "usage: aws elevate status") {
		t.Errorf("bad flag: exit %d, %q", got, h.errOut.String())
	}
	h = newHarness(t)
	if got := h.run("request", "--duration", "soon"); got != 2 || !strings.Contains(h.errOut.String(), "45m or 2h") {
		t.Errorf("bad duration: exit %d, %q", got, h.errOut.String())
	}
	h = newHarness(t)
	if got := h.run("request", "a", "b"); got != 2 {
		t.Errorf("two rules: exit %d", got)
	}
}

func TestShellEnvDropsWhatOverridesTheProfile(t *testing.T) {
	got := shellEnv([]string{"A=1", "AWS_PROFILE=x", "AWS_DEFAULT_PROFILE=y", "AWS_SESSION_TOKEN=t", "AWS_REGION=eu-west-1"}, "jit")
	if strings.Join(got, ",") != "A=1,AWS_REGION=eu-west-1,AWS_PROFILE=jit" {
		t.Errorf("env %v", got)
	}
}

func lineWith(text, part string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, part) {
			return l
		}
	}
	return ""
}

func TestRequestWithoutAccountUsesTheOnlyOneOfTheRule(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.svc.rules = []api.Rule{rule}
	h.svc.grant = grantFor("abcde12345")
	h.svc.states = []string{api.StateActive}
	if got := h.run("request", "ssm-prod", "--reason", "x", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	if h.svc.created.Account != "222222222222" {
		t.Errorf("created %+v", *h.svc.created)
	}
}

func TestRequestWithoutAccountOnSeveralSaysWhichToAdd(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	several := rule
	several.Accounts = []api.Ref{{ID: "222222222222", Name: "prod"}, {ID: "333333333333", Name: "stage"}}
	h.svc.rules = []api.Rule{several}
	if got := h.run("request", "ssm-prod", "--reason", "x", "--profile", "work"); got == 0 {
		t.Fatal("exit 0")
	}
	if h.svc.created != nil {
		t.Error("sent to the service")
	}
	want := "ssm-prod has several accounts: add --account with one of prod (222222222222), stage (333333333333)"
	if !strings.Contains(h.errOut.String(), want) {
		t.Errorf("stderr %q", h.errOut.String())
	}
}

func TestAnExpiredSessionSaysOnlyWhatToRun(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.env.Open = func(context.Context, string) (Backend, error) {
		return nil, fmt.Errorf("get ElevateAdmin credentials: %w",
			&awsx.SessionExpiredError{Profile: "work", Err: errors.New("operation error SSO OIDC: CreateToken, StatusCode: 400")})
	}
	if got := h.run("status", "--profile", "work"); got != 1 {
		t.Fatalf("exit %d", got)
	}
	want := "aws-elevate: your AWS session for profile work has expired. Run:\n  aws sso login --profile work\n"
	if h.errOut.String() != want {
		t.Errorf("stderr %q", h.errOut.String())
	}
}

func TestCommandsUseTheSetUpProfileByDefault(t *testing.T) {
	h := newHarness(t)
	if err := awsx.SaveSettings(h.env.SettingsPath, awsx.Settings{
		Profiles: map[string]awsx.Target{"work": target}, Default: "work",
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.run("status"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
}

func TestProfileFlagAndEnvWinOverTheDefault(t *testing.T) {
	h := newHarness(t)
	if err := awsx.SaveSettings(h.env.SettingsPath, awsx.Settings{
		Profiles: map[string]awsx.Target{"work": target}, Default: "work",
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.run("status", "--profile", "other"); got != 1 || !strings.Contains(h.errOut.String(), `profile "other" is not set up`) {
		t.Errorf("flag: exit %d, %q", got, h.errOut.String())
	}
	h.errOut.Reset()
	h.vars["AWS_PROFILE"] = "env"
	if got := h.run("status"); got != 1 || !strings.Contains(h.errOut.String(), `profile "env" is not set up`) {
		t.Errorf("env: exit %d, %q", got, h.errOut.String())
	}
	if !strings.Contains(h.errOut.String(), "set up: work") {
		t.Errorf("no hint of the set-up profiles: %q", h.errOut.String())
	}
}

func TestSetupSavesTheProfileAsDefault(t *testing.T) {
	h := newHarness(t)
	if got := h.run("setup", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	s, err := awsx.LoadSettings(h.env.SettingsPath)
	if err != nil || s.Default != "work" {
		t.Fatalf("default %q, %v", s.Default, err)
	}
	if !strings.Contains(h.out.String(), "✓ Profile work saved as default for aws-elevate") {
		t.Errorf("output %q", h.out.String())
	}
	if strings.Contains(h.out.String(), "not on your PATH") {
		t.Errorf("warned while on the PATH: %q", h.out.String())
	}
	if !strings.HasSuffix(h.out.String(), "  Next: aws elevate request\n") {
		t.Errorf("last line: %q", h.out.String())
	}
}

func TestSetupWarnsWhenNotOnPath(t *testing.T) {
	h := newHarness(t)
	h.env.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if got := h.run("setup", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	for _, want := range []string{
		`! aws-elevate is not on your PATH: the alias "aws elevate" cannot find it`,
		"Fix: move it to ~/.local/bin, or run install.sh",
	} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in %q", want, h.out.String())
		}
	}
}

func TestSetupNextIncludesTheProfileWhenNotDefault(t *testing.T) {
	h := newHarness(t)
	h.vars["AWS_PROFILE"] = "other"
	h.svc.roles = []string{"auditor"}
	if got := h.run("setup", "--profile", "work"); got != 0 {
		t.Fatalf("exit %d: %s", got, h.errOut.String())
	}
	if !strings.Contains(h.out.String(), "Next: aws elevate ui --profile work") {
		t.Errorf("output %q", h.out.String())
	}
}
