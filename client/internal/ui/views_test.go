package ui

import (
	"context"
	"encoding/csv"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// fakeService answers like the service, from fixed data. A grant's actions are set by the
// test: the views must show only those.
type fakeService struct {
	rules       []api.Rule
	permsErr    error
	lists       map[string][]api.Grant // by scope
	pages       int                    // with more than 0, Grants answers this many pages
	grants      map[string]api.Grant
	timeline    []api.TimelineEntry
	created     *api.CreateRequest
	createErr   error
	calls       []string
	queries     []api.ListQuery
	revokeErr   map[string]error
	versions    *api.ConfigVersions
	docs        map[int]string // rules documents by version
	afterAction string         // state returned by an action
}

func (f *fakeService) RequestableRules(context.Context) (*api.RuleList, error) {
	f.calls = append(f.calls, "requestable rules")
	return &api.RuleList{ConfigVersion: 3, Rules: f.rules}, nil
}

func (f *fakeService) Permissions(_ context.Context, id string) (*api.Permissions, error) {
	f.calls = append(f.calls, "permissions "+id)
	if f.permsErr != nil {
		return nil, f.permsErr
	}
	return &api.Permissions{
		Managed:       []string{"arn:aws:iam::aws:policy/AmazonSSMReadOnlyAccess"},
		Inline:        []byte(`{"Version":"2012-10-17"}`),
		InlineSummary: []api.StatementSummary{{Effect: "Allow", Actions: []string{"ssm:PutParameter"}, Resources: []string{"arn:aws:ssm:*:*:parameter/app/*"}}},
	}, nil
}

func (f *fakeService) PermissionSetPermissions(ctx context.Context, arn string) (*api.Permissions, error) {
	f.calls = append(f.calls, "ps permissions "+arn)
	return f.Permissions(ctx, "")
}

func (f *fakeService) CreateGrant(_ context.Context, req api.CreateRequest) (*api.Grant, error) {
	f.created = &req
	if f.createErr != nil {
		return nil, f.createErr
	}
	g := f.grants["new4567890"]
	return &g, nil
}

func (f *fakeService) Grant(_ context.Context, id string) (*api.GrantDetail, error) {
	f.calls = append(f.calls, "get "+id)
	g, ok := f.grants[id]
	if !ok {
		return nil, &api.Error{Status: 404, Code: "not_found", Message: "no grant " + id}
	}
	return &api.GrantDetail{Grant: g, Timeline: f.timeline}, nil
}

func (f *fakeService) Grants(_ context.Context, q api.ListQuery) (*api.GrantList, error) {
	f.queries = append(f.queries, q)
	items := f.lists[q.Scope]
	if f.pages > 0 {
		n := 0
		if q.Cursor != "" {
			n = int(q.Cursor[0] - '0')
		}
		var next *string
		if n+1 < f.pages {
			c := string(rune('0' + n + 1))
			next = &c
		}
		return &api.GrantList{Items: items, Cursor: next}, nil
	}
	return &api.GrantList{Items: items}, nil
}

func (f *fakeService) act(name, id string) (*api.Grant, error) {
	f.calls = append(f.calls, name+" "+id)
	if err := f.revokeErr[id]; err != nil {
		return nil, err
	}
	g := f.grants[id]
	g.State = f.afterAction
	return &g, nil
}

func (f *fakeService) Approve(_ context.Context, id string) (*api.Grant, error) {
	return f.act("approve", id)
}
func (f *fakeService) Reject(_ context.Context, id string) (*api.Grant, error) {
	return f.act("reject", id)
}
func (f *fakeService) Cancel(_ context.Context, id string) (*api.Grant, error) {
	return f.act("cancel", id)
}
func (f *fakeService) Revoke(_ context.Context, id, reason string) (*api.Grant, error) {
	return f.act("revoke "+reason, id)
}

var (
	me    = api.User{UserID: "u-1", UserName: "user@example.com"}
	other = api.User{UserID: "u-2", UserName: "other@example.com"}
	leads = &api.Ref{ID: "g-lead", Name: "leads"}
)

func ruleSSM() api.Rule {
	return api.Rule{
		ID: "ssm-prod", Group: api.Ref{ID: "g-dev", Name: "devs"},
		PermissionSet: api.PermissionSet{Arn: "arn:aws:sso:::permissionSet/ssoins-1/ps-1", Name: "ssm-write"},
		Accounts:      []api.Ref{{ID: "222222222222", Name: "prod"}},
		Duration:      api.Duration{DefaultSeconds: 3600, MaxSeconds: 14400},
		Approval:      "manual", ApproverGroup: leads,
	}
}

func grant(id, state string, who api.User, actions ...string) api.Grant {
	exp := time.Now().Add(38 * time.Minute)
	return api.Grant{
		ID: id, State: state, RuleID: "ssm-prod", Approval: "manual", Requester: who, ApproverGroup: leads,
		SourcePs: api.PermissionSet{Name: "ssm-write"}, Account: "222222222222", DurationSeconds: 3600,
		Reason: "incident 42", CreatedAt: time.Now().Add(-5 * time.Minute), ExpiresAt: &exp, Actions: actions,
	}
}

type harness struct {
	svc   *fakeService
	app   *App
	ready []string
}

func newHarness(roles ...string) *harness {
	h := &harness{svc: &fakeService{rules: []api.Rule{ruleSSM()}, grants: map[string]api.Grant{}, lists: map[string][]api.Grant{}}}
	h.app = &App{
		Me:      api.Me{User: me, Roles: roles},
		Service: h.svc,
		Ready: func(_ context.Context, g api.Grant) (string, error) {
			h.ready = append(h.ready, g.ID)
			return "jit-ssm-write-222222222222", nil
		},
		Console: func(g api.Grant) string { return "https://example.awsapps.com/start/#/console?account_id=" + g.Account },
	}
	return h
}

func (h *harness) do(t *testing.T, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	w := httptest.NewRecorder()
	Pages(h.app).ServeHTTP(w, r)
	return w
}

func (h *harness) get(t *testing.T, target string) string {
	t.Helper()
	w := h.do(t, "GET", target, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, w.Code, w.Body)
	}
	return w.Body.String()
}

func contains(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}

func lacks(t *testing.T, out string, nots ...string) {
	t.Helper()
	for _, not := range nots {
		if strings.Contains(out, not) {
			t.Errorf("unexpected %s in\n%s", not, out)
		}
	}
}

func TestEveryUserViewAnswers(t *testing.T) {
	h := newHarness("requester")
	for _, path := range []string{"/request", "/request?rule=ssm-prod", "/active", "/pending", "/history"} {
		out := h.get(t, path)
		contains(t, out, `class="nav"`, `aria-current="page"`)
	}
	if w := h.do(t, "GET", "/", nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/request" {
		t.Errorf("home: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestNavigationFollowsTheRoles(t *testing.T) {
	out := newHarness("requester").get(t, "/active")
	contains(t, out, `href="/request"`, `href="/active"`, `href="/pending"`, `href="/history"`, `user@example.com · requester`)

	auditor := newHarness("auditor")
	out = auditor.get(t, "/active")
	lacks(t, out, `href="/request"`)
	if w := auditor.do(t, "GET", "/request", nil); w.Code != http.StatusNotFound {
		t.Errorf("request view for an auditor: %d", w.Code)
	}
	if w := auditor.do(t, "GET", "/", nil); w.Header().Get("Location") != "/active" {
		t.Errorf("auditor home: %s", w.Header().Get("Location"))
	}
}

func TestServiceErrorsAreShownOnThePage(t *testing.T) {
	h := newHarness("requester")
	h.svc.grants = nil
	w := h.do(t, "GET", "/request/abcdef2345", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("code %d", w.Code)
	}
	contains(t, w.Body.String(), `class="alert error"`, "not_found: no grant abcdef2345", `class="nav"`)
}

// --- Request ---

func TestRequestListsTheRulesAndShowsTheFormOfTheChosenOne(t *testing.T) {
	h := newHarness("requester")
	out := h.get(t, "/request")
	contains(t, out, `href="/request?rule=ssm-prod"`, "ssm-write", "Choose what to request")
	lacks(t, out, `<textarea`)
	// Admins and auditors see every rule elsewhere; Request shows only what can be asked.
	if h.svc.calls[0] != "requestable rules" {
		t.Errorf("calls %v", h.svc.calls)
	}

	out = h.get(t, "/request?rule=ssm-prod")
	contains(t, out,
		`<form method="post" action="/request">`,
		`<input type="hidden" name="rule" value="ssm-prod">`,
		`<option value="222222222222" selected>prod (222222222222)</option>`,
		`name="duration"`, `placeholder="1h"`, "Default 1h, at most 4h",
		`<textarea name="reason"`,
		"Approval by leads",
		`data-load="/request/permissions?rule=ssm-prod"`,
	)
	// "What you will get" is read only when opened.
	for _, c := range h.svc.calls {
		if strings.HasPrefix(c, "permissions") {
			t.Errorf("permissions read before opening: %v", h.svc.calls)
		}
	}
}

func TestWhatYouWillGetIsAFragment(t *testing.T) {
	h := newHarness("requester")
	out := h.get(t, "/request/permissions?rule=ssm-prod")
	contains(t, out, "AmazonSSMReadOnlyAccess", "ssm:PutParameter", "arn:aws:ssm:*:*:parameter/app/*", "&#34;Version&#34;: &#34;2012-10-17&#34;")
	lacks(t, out, `class="nav"`)

	h.svc.permsErr = &api.Error{Status: 404, Code: "not_found", Message: "permission set ssm-write no longer exists"}
	w := h.do(t, "GET", "/request/permissions?rule=ssm-prod", nil)
	if w.Code != http.StatusOK {
		t.Errorf("code %d", w.Code)
	}
	contains(t, w.Body.String(), `class="alert error"`, "permission set ssm-write no longer exists")
}

func TestSubmittingARequestGoesToItsProgress(t *testing.T) {
	h := newHarness("requester")
	h.svc.grants["new4567890"] = grant("new4567890", "activating", me)
	w := h.do(t, "POST", "/request", url.Values{"rule": {"ssm-prod"}, "account": {"222222222222"}, "duration": {"45m"}, "reason": {"incident 42"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/request/new4567890" {
		t.Fatalf("%d %s %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	want := api.CreateRequest{RuleID: "ssm-prod", Account: "222222222222", DurationSeconds: 2700, Reason: "incident 42"}
	if *h.svc.created != want {
		t.Errorf("created %+v", *h.svc.created)
	}
}

func TestARefusedRequestKeepsTheForm(t *testing.T) {
	h := newHarness("requester")
	h.svc.createErr = &api.Error{Status: 400, Code: "invalid_request", Message: "duration over the rule maximum"}
	w := h.do(t, "POST", "/request", url.Values{"rule": {"ssm-prod"}, "account": {"222222222222"}, "duration": {"9h"}, "reason": {"incident <42>"}})
	if w.Code != http.StatusOK {
		t.Fatalf("code %d", w.Code)
	}
	contains(t, w.Body.String(), "duration over the rule maximum", `value="9h"`, ">incident &lt;42&gt;</textarea>")

	w = h.do(t, "POST", "/request", url.Values{"rule": {"ssm-prod"}, "duration": {"soon"}, "reason": {"x"}})
	contains(t, w.Body.String(), `class="alert error"`, "like 45m or 2h")
}

func TestProgressWaitsForTheRoleBeforeTheConsole(t *testing.T) {
	h := newHarness("requester")
	h.svc.grants["new4567890"] = grant("new4567890", "pending", me)
	out := h.get(t, "/request/new4567890")
	contains(t, out, `data-poll="/request/new4567890/progress"`, "Waiting for approval by leads")
	lacks(t, out, "console?account_id")

	g := grant("new4567890", "active", me)
	h.svc.grants["new4567890"] = g
	// The page itself does not wait: the poll does.
	out = h.get(t, "/request/new4567890")
	contains(t, out, `data-poll=`, "Waiting for AWS to accept the role")
	lacks(t, out, `href="https://example.awsapps.com`)
	if len(h.ready) != 0 {
		t.Errorf("waited on the page: %v", h.ready)
	}

	out = h.get(t, "/request/new4567890/progress")
	if len(h.ready) != 1 {
		t.Fatalf("ready calls %v", h.ready)
	}
	contains(t, out,
		"aws elevate shell new4567890", "export AWS_PROFILE=jit-ssm-write-222222222222",
		`<a class="btn primary" href="https://example.awsapps.com/start/#/console?account_id=222222222222" target="_blank" rel="noopener noreferrer">Open in console</a>`,
	)
	lacks(t, out, "data-poll", `class="nav"`)
}

func TestProgressOfAClosedRequest(t *testing.T) {
	h := newHarness("requester")
	g := grant("new4567890", "closed", me)
	reason, msg := "failed", "permission set ssm-write not found"
	g.CloseReason, g.Error = &reason, &msg
	h.svc.grants["new4567890"] = g
	out := h.get(t, "/request/new4567890/progress")
	contains(t, out, "Closed", "failed", msg)
	lacks(t, out, "data-poll")

	h.app.Ready = func(context.Context, api.Grant) (string, error) { return "", errors.New("role not accepted after 60s") }
	h.svc.grants["new4567890"] = grant("new4567890", "active", me)
	out = h.get(t, "/request/new4567890/progress")
	contains(t, out, `class="alert error"`, "role not accepted after 60s", "aws elevate shell new4567890")
}

// --- Active ---

func activeHarness() *harness {
	h := newHarness("requester", "approver")
	mine := grant("mine234567", "active", me, "revoke")
	theirs := grant("their23456", "active", other, "revoke")
	theirs.Privileged = true
	theirs.RuleRemoved = true
	watched := grant("watch23456", "revoking", other) // visible, cannot be revoked by the caller
	h.svc.lists["mine"] = []api.Grant{mine, grant("pend234567", "pending", me, "cancel")}
	h.svc.lists["approvable"] = []api.Grant{theirs, mine, watched}
	for _, g := range []api.Grant{mine, theirs, watched} {
		h.svc.grants[g.ID] = g
	}
	return h
}

func TestActiveShowsOpenGrantsWithOnlyTheirActions(t *testing.T) {
	h := activeHarness()
	out := h.get(t, "/active")
	contains(t, out,
		`<span class="counter">(3)</span>`,
		`value="mine234567"`, `value="their23456"`,
		`formaction="/active/revoke"`,
		`<span class="tag">privileged</span>`, `<span class="tag">rule removed</span>`,
		`data-poll="/active/row/watch23456?check=1"`,
		`href="https://example.awsapps.com/start/#/console?account_id=222222222222"`,
	)
	lacks(t, out, `value="watch23456"`, "pend234567")
	if strings.Count(out, "console?account_id") != 1 {
		t.Errorf("console link on grants of others:\n%s", out)
	}

	out = h.get(t, "/active?f=mine")
	contains(t, out, `<span class="counter">(1)</span>`, `aria-current="true">Mine (1)</a>`)

	// Admins also see every open grant.
	admin := newHarness("admin")
	admin.get(t, "/active")
	scopes := []string{}
	for _, q := range admin.svc.queries {
		scopes = append(scopes, q.Scope)
	}
	if strings.Join(scopes, ",") != "mine,all" {
		t.Errorf("admin scopes %v", scopes)
	}
}

func TestActiveWithoutRevocableGrantsHasNoActions(t *testing.T) {
	h := newHarness("requester")
	h.svc.lists["mine"] = []api.Grant{grant("watch23456", "revoking", me)}
	out := h.get(t, "/active")
	lacks(t, out, "data-action", "checkbox")
}

func TestARowInTransitionIsAPollingFragment(t *testing.T) {
	h := activeHarness()
	// The row keeps the selection column of its table.
	out := h.get(t, "/active/row/watch23456?check=1")
	if !strings.HasPrefix(out, "<tr") {
		t.Errorf("not a row: %s", out)
	}
	contains(t, out, `data-poll="/active/row/watch23456?check=1"`, "Revoking", `<td class="check"></td>`)
	out = h.get(t, "/active/row/watch23456")
	contains(t, out, `data-poll="/active/row/watch23456"`)
	lacks(t, out, `class="check"`)
	h.svc.grants["watch23456"] = grant("watch23456", "closed", other)
	out = h.get(t, "/active/row/watch23456?check=1")
	lacks(t, out, "data-poll")
}

func TestRevokeAsksForReasonAndShowsEachOutcome(t *testing.T) {
	h := activeHarness()
	out := h.get(t, "/active/revoke?id=mine234567&id=their23456")
	contains(t, out, `<form method="post" action="/active/revoke">`,
		`<input type="hidden" name="id" value="mine234567">`, `<input type="hidden" name="id" value="their23456">`,
		`<textarea name="reason"`, "Revoke 2 grants")

	w := h.do(t, "POST", "/active/revoke", url.Values{"id": {"mine234567", "their23456"}, "reason": {"  "}})
	contains(t, w.Body.String(), `class="alert error"`, "A reason is required", `value="their23456"`)
	if len(h.svc.calls) != 0 {
		t.Fatalf("revoked without a reason: %v", h.svc.calls)
	}

	h.svc.afterAction = "revoking"
	h.svc.revokeErr = map[string]error{"their23456": &api.Error{Status: 409, Code: "state_conflict", Message: "grant their23456 is closed"}}
	w = h.do(t, "POST", "/active/revoke", url.Values{"id": {"mine234567", "their23456"}, "reason": {"done early"}})
	out = w.Body.String()
	if strings.Join(h.svc.calls, ",") != "revoke done early mine234567,revoke done early their23456" {
		t.Errorf("calls %v", h.svc.calls)
	}
	contains(t, out, "Revocation results", "mine234567", `data-poll="/active/outcome/mine234567"`, "state_conflict: grant their23456 is closed")

	h.svc.grants["mine234567"] = grant("mine234567", "revoking", me)
	out = h.get(t, "/active/outcome/mine234567")
	contains(t, out, `data-poll="/active/outcome/mine234567"`)
	lacks(t, out, `class="check"`)
}

// --- Pending ---

func TestPendingShowsRequestsToApproveAndOwnRequests(t *testing.T) {
	h := newHarness("requester", "approver")
	ask := grant("ask2345678", "pending", other, "approve", "reject")
	ask.Privileged = true
	exp := time.Now().Add(26 * time.Minute)
	ask.ApprovalExpiresAt = &exp
	seen := grant("seen234567", "pending", other) // approvable scope, no action for the caller
	own := grant("own2345678", "pending", me, "cancel")
	h.svc.lists["approvable"] = []api.Grant{ask, seen, grant("act2345678", "active", other, "revoke")}
	h.svc.lists["mine"] = []api.Grant{own, grant("mine234567", "active", me, "revoke")}
	out := h.get(t, "/pending")
	contains(t, out,
		`To approve <span class="counter">(1)</span>`, `Your requests <span class="counter">(1)</span>`,
		"other@example.com", "incident 42", `<span class="tag">privileged</span>`,
		`<button class="btn primary" formaction="/pending/approve"`, `href="/pending/reject?id=ask2345678"`,
		`href="/pending/cancel?id=own2345678"`,
		`data-load="/request/permissions?rule=ssm-prod"`,
		`data-load="/pending/history?user=u-2"`,
	)
	lacks(t, out, "seen234567", "act2345678", "mine234567", `formaction="/pending/approve" name="id" value="own2345678"`)
}

func TestPendingActionsShowTheirOutcome(t *testing.T) {
	h := newHarness("approver")
	h.svc.grants["ask2345678"] = grant("ask2345678", "pending", other, "approve", "reject")
	h.svc.afterAction = "activating"
	w := h.do(t, "POST", "/pending/approve", url.Values{"id": {"ask2345678"}})
	contains(t, w.Body.String(), `class="alert success"`, "ask2345678", "Activating")

	out := h.get(t, "/pending/reject?id=ask2345678")
	contains(t, out, `<form method="post" action="/pending/reject">`, `value="ask2345678"`, "Reject request")
	h.svc.afterAction = "closed"
	w = h.do(t, "POST", "/pending/reject", url.Values{"id": {"ask2345678"}})
	contains(t, w.Body.String(), "ask2345678", "Closed")
	if strings.Join(h.svc.calls, ",") != "approve ask2345678,get ask2345678,reject ask2345678" {
		t.Errorf("calls %v", h.svc.calls)
	}
}

func TestRequesterHistoryUsesTheWidestScopeTheRoleAllows(t *testing.T) {
	for roles, scope := range map[string]string{"approver": "approvable", "approver,auditor": "all"} {
		h := newHarness(strings.Split(roles, ",")...)
		h.svc.lists[scope] = []api.Grant{grant("old2345678", "closed", other)}
		out := h.get(t, "/pending/history?user=u-2")
		contains(t, out, "old2345678")
		lacks(t, out, `class="nav"`)
		if q := h.svc.queries[0]; q.Scope != scope || q.User != "u-2" {
			t.Errorf("%s: query %+v", roles, q)
		}
	}
}

// --- History ---

func TestHistoryPagesReplaceAndTheDetailOpensAside(t *testing.T) {
	h := newHarness("requester")
	h.svc.pages = 2
	h.svc.lists["mine"] = []api.Grant{grant("old2345678", "closed", me)}
	out := h.get(t, "/history")
	contains(t, out, `href="/history?period=30d&amp;scope=mine&amp;cursor=1"`, `href="/history?period=30d&amp;scope=mine&amp;open=old2345678"`,
		`href="/history/export.csv?period=30d&amp;scope=mine"`, "30 days", "13 months")
	lacks(t, out, "Everyone", `class="box detail"`)
	if q := h.svc.queries[0]; q.Scope != "mine" || q.From == "" || q.To != "" {
		t.Errorf("query %+v", q)
	}

	at := time.Now()
	h.svc.grants["old2345678"] = grant("old2345678", "closed", me)
	h.svc.timeline = []api.TimelineEntry{{At: at, State: "pending"}, {At: at, State: "closed"}}
	out = h.get(t, "/history?period=90d&cursor=1&open=old2345678")
	contains(t, out, `class="box detail"`, "old2345678", "incident 42")
	if q := h.svc.queries[1]; q.Cursor != "1" {
		t.Errorf("cursor not sent: %+v", q)
	}

	auditor := newHarness("auditor")
	out = auditor.get(t, "/history?scope=all")
	contains(t, out, `aria-current="true">Everyone</a>`)
	if q := auditor.svc.queries[0]; q.Scope != "all" {
		t.Errorf("auditor query %+v", q)
	}
}

func TestExportReadsEveryPageAndNeutralisesFormulas(t *testing.T) {
	h := newHarness("requester")
	h.svc.pages = 3
	g := grant("old2345678", "closed", me)
	g.Reason = "=HYPERLINK(\"x\")"
	h.svc.lists["mine"] = []api.Grant{g}
	w := h.do(t, "GET", "/history/export.csv?period=13m&scope=mine", nil)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[0][0] != "id" {
		t.Fatalf("rows %v", rows)
	}
	if !strings.Contains(strings.Join(rows[1], ","), "'=HYPERLINK") {
		t.Errorf("formula not neutralised: %v", rows[1])
	}
	if len(h.svc.queries) != 3 {
		t.Errorf("pages read %d", len(h.svc.queries))
	}
}
