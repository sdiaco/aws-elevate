package ui

import (
	"bytes"
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// fakeService answers like the service, from fixed data. A grant's actions are set by the
// test: the views must show only those.
type fakeService struct {
	rules       []api.Rule
	openGrants  []api.OpenGrant
	rulesErr    error // GET /rules fails, as with no valid rules version
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
	me          *api.Me        // answer of GET /me when the interface refreshes the roles
	meErr       error
}

func (f *fakeService) Me(context.Context) (*api.Me, error) {
	f.calls = append(f.calls, "me")
	if f.meErr != nil {
		return nil, f.meErr
	}
	return f.me, nil
}

func (f *fakeService) RequestableRules(context.Context) (*api.RuleList, error) {
	f.calls = append(f.calls, "requestable rules")
	if f.rulesErr != nil {
		return nil, f.rulesErr
	}
	return &api.RuleList{ConfigVersion: 3, Rules: f.rules, OpenGrants: f.openGrants}, nil
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
func (f *fakeService) Reject(_ context.Context, id, reason string) (*api.Grant, error) {
	return f.act("reject "+reason, id)
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

// --- History ---

func TestHistoryPagesReplaceAndTheDetailOpensAside(t *testing.T) {
	h := newHarness("requester")
	h.svc.pages = 2
	h.svc.lists["mine"] = []api.Grant{grant("old2345678", "closed", me)}
	out := h.get(t, "/history")
	contains(t, out, `href="/history?period=30d&amp;scope=mine&amp;cursor=1"`, `href="/history?period=30d&amp;scope=mine&amp;open=old2345678"`,
		`href="/history/export.csv?period=30d&amp;scope=mine"`, "30 days", "13 months")
	contains(t, out, `<section class="split-panel" data-panel-slot hidden></section>`)
	lacks(t, out, "Everyone", `class="open"`)
	if q := h.svc.queries[0]; q.Scope != "mine" || !q.Closed || q.From == "" || q.To != "" {
		t.Errorf("query %+v", q)
	}

	at := time.Now()
	h.svc.grants["old2345678"] = grant("old2345678", "closed", me)
	h.svc.timeline = []api.TimelineEntry{{At: at, State: "pending"}, {At: at, State: "closed"}}
	out = h.get(t, "/history?period=90d&cursor=1&open=old2345678")
	contains(t, out, `class="split-panel"`, "old2345678", "incident 42")
	if q := h.svc.queries[1]; q.Cursor != "1" {
		t.Errorf("cursor not sent: %+v", q)
	}

	auditor := newHarness("auditor")
	out = auditor.get(t, "/history?scope=all")
	contains(t, out, `aria-current="true">Everyone</a>`)
	if q := auditor.svc.queries[0]; q.Scope != "all" || !q.Closed {
		t.Errorf("auditor query %+v", q)
	}
}

func TestHistoryOpensOnEveryoneForWhoNeverRequests(t *testing.T) {
	// "Mine" is always empty for a user who cannot request.
	for roles, want := range map[string]string{"auditor": "all", "admin": "all", "auditor,requester": "mine"} {
		h := newHarness(strings.Split(roles, ",")...)
		h.get(t, "/history")
		if q := h.svc.queries[0]; q.Scope != want {
			t.Errorf("%s: scope %q, want %q", roles, q.Scope, want)
		}
		h.get(t, "/history?scope=mine")
		if q := h.svc.queries[1]; q.Scope != "mine" {
			t.Errorf("%s: chosen Mine became %q", roles, q.Scope)
		}
	}
}

func TestARejectedGrantShowsWhoRejectedItAndWhy(t *testing.T) {
	h := newHarness("requester")
	g := grant("rej2345678", "closed", me)
	reason, why := "rejected", "use the read-only rule"
	closed := time.Now()
	g.CloseReason, g.RejectReason, g.ClosedAt = &reason, &why, &closed
	g.Approver = &api.User{UserID: "u-2", UserName: "lead@example.com"}
	h.svc.lists["mine"] = []api.Grant{g}
	h.svc.grants["rej2345678"] = g
	out := h.get(t, "/history?open=rej2345678")
	contains(t, out, "<dt>Rejected by</dt><dd>lead@example.com: use the read-only rule</dd>")
	lacks(t, out, "Approved by")
}

func periodLabels(t *testing.T, h *harness) (out string) {
	t.Helper()
	return h.get(t, "/history")
}

func TestHistoryLongestPeriodUsesTheServiceMonths(t *testing.T) {
	h := newHarness("requester")
	h.app.Me.Limits.HistoryMonths = 24
	out := periodLabels(t, h)
	contains(t, out, "24 months")
	contains(t, out, "period=all")
	lacks(t, out, "13 months")
	h.get(t, "/history?period=all")
	got, err := time.Parse(time.RFC3339, h.svc.queries[len(h.svc.queries)-1].From)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Now().AddDate(0, -24, 0); got.Sub(want) > time.Minute || want.Sub(got) > time.Minute {
		t.Errorf("from %s, want about %s", got, want)
	}
}

func TestHistoryPeriodsFallBackToThirteenMonths(t *testing.T) {
	h := newHarness("requester")
	contains(t, periodLabels(t, h), "13 months")
}

func TestHistoryTakesAnOldBookmarkAsTheLongestPeriod(t *testing.T) {
	h := newHarness("requester")
	h.app.Me.Limits.HistoryMonths = 24
	out := h.get(t, "/history?period=13m")
	if !strings.Contains(out, "period=all") {
		t.Errorf("no all period:\n%s", out)
	}
	got, _ := time.Parse(time.RFC3339, h.svc.queries[len(h.svc.queries)-1].From)
	if time.Since(got) < 700*24*time.Hour {
		t.Errorf("from %s", got)
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

func TestHistoryShowsTheOutcomeSecondAndTheRequesterOnlyForOthersRows(t *testing.T) {
	h := newHarness("requester")
	rejected, expired := grant("rej2345678", "closed", me), grant("exp2345678", "closed", me)
	r, e := "rejected", "timed_out"
	rejected.CloseReason, expired.CloseReason = &r, &e
	h.svc.lists["mine"] = []api.Grant{rejected, expired}
	out := h.get(t, "/history")
	// One coloured label per outcome, right after the id, so it shows on a phone too.
	contains(t, out, `<th>Id</th><th>Outcome</th>`, `class="st st-outcome-red">Rejected</span>`,
		`class="st st-outcome">Not approved in time</span>`)
	// Only the user's own rows: the requester column would repeat their name.
	lacks(t, out, `<th>Requester</th>`, ">Closed<")

	auditor := newHarness("auditor")
	auditor.svc.lists["all"] = []api.Grant{rejected}
	contains(t, auditor.get(t, "/history"), `<th>Requester</th>`)
}

func TestRequestsShowTheRequesterOnlyToWhoSeesOthersRequests(t *testing.T) {
	lacks(t, requestsHarness("requester").get(t, "/requests"), `<th>Requester</th>`)
	contains(t, requestsHarness("requester", "approver").get(t, "/requests"), `<th>Requester</th>`)
}

func TestAnUnknownAddressShowsAPageInTheInterface(t *testing.T) {
	h := newHarness("auditor")
	for _, path := range []string{"/rules/new", "/requests/new", "/rules/delete"} {
		w := h.do(t, "GET", path, nil)
		if w.Code != 404 {
			t.Errorf("%s: %d", path, w.Code)
		}
		contains(t, w.Body.String(), `class="nav"`, "Page not found", `href="/requests"`)
	}
}

func TestEveryPageCarriesTheNoticeForAClosedInterface(t *testing.T) {
	h := newHarness("requester")
	h.app.Reopen = "aws elevate ui --profile work"
	out := h.get(t, "/requests")
	contains(t, out, `<template id="closed-alert">`,
		"This interface has stopped (after 30 minutes without use, or from the terminal). Run this command to open it again:",
		"aws elevate ui --profile work")
}

func TestAccountsShowTheNameTheyHadWhenRequested(t *testing.T) {
	h := newHarness("requester")
	named := grant("new2345678", "active", me, "revoke")
	named.AccountName = "prod"
	older := grant("old2345678", "active", me, "revoke")
	h.svc.lists["mine"] = []api.Grant{named, older}
	h.svc.grants["new2345678"] = named
	out := h.get(t, "/requests?open=new2345678")
	contains(t, out,
		`<span>prod</span><span class="cell-sub">222222222222</span>`,           // the row
		`<dt>Account</dt><dd>prod <span class="muted">222222222222</span></dd>`, // the panel
		`<span>222222222222</span>`,                                             // a grant from before names were kept
	)

	h.svc.lists["mine"] = []api.Grant{named}
	w := h.do(t, "GET", "/history/export.csv?period=13m&scope=mine", nil)
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(rows[0], "accountName")
	if i < 0 || rows[1][i] != "prod" || rows[0][i-1] != "account" {
		t.Errorf("header %v, row %v", rows[0], rows[1])
	}
}

// reasonPages are every page and fragment that renders a reason textarea: the request
// form (whole page and fetched fragment) and the reject and revoke confirmations.
var reasonPages = []string{
	"/requests/new?rule=ssm-prod",
	"/requests/new/form?rule=ssm-prod",
	"/requests/reject?id=ask2345678",
	"/requests/revoke?id=mine234567",
}

func reasonHarness(limit int) *harness {
	h := requestsHarness("requester", "approver")
	h.app.Me.Limits.ReasonChars = limit
	return h
}

func TestReasonFieldsCarryTheLimitAndTheHint(t *testing.T) {
	h := reasonHarness(321)
	for _, page := range reasonPages {
		out := h.get(t, page)
		if n := strings.Count(out, `<textarea name="reason"`); n != 1 {
			t.Fatalf("%s: %d reason fields", page, n)
		}
		contains(t, out, ` maxlength="321" data-count`,
			`</textarea>`+"\n"+`<p class="field-hint">Do not include personal data<span data-counter></span></p>`)
		lacks(t, out, "500")
	}
}

func TestReasonFieldWithoutALimitFromAnOlderService(t *testing.T) {
	h := reasonHarness(0)
	for _, page := range reasonPages {
		out := h.get(t, page)
		contains(t, out, `<textarea name="reason"`, `<p class="field-hint">Do not include personal data</p>`)
		lacks(t, out, "maxlength", "data-count", "data-counter")
	}
}

func TestEveryReasonFieldIsBuiltByTheServer(t *testing.T) {
	js, err := files.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	lacks(t, string(js), `"textarea"`, "createElement(\"textarea", "<textarea")
}

// busyOf returns the data-busy of the submit button with this label, or "" when the page
// has none.
func busyOf(out, label string) string {
	m := regexp.MustCompile(`<button[^>]*data-busy="([^"]*)"[^>]*>` + regexp.QuoteMeta(label) + `</button>`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestSubmitButtonsSayWhatTheyWaitFor(t *testing.T) {
	req := requestsHarness("requester", "approver")
	rules, _ := rulesHarness()
	review := rules.do(t, "POST", "/rules/edit", postedRule("db-prod")).Body.String()
	replacing := rules.do(t, "POST", "/rules/publish", url.Values{"change": {hidden(review, "change")}, "latest": {hidden(review, "latest")}, "replaces": {"ssm-dev"}}).Body.String()
	deleting := rules.do(t, "POST", "/rules/delete", url.Values{"id": {"ssm-prod"}}).Body.String()
	first := freshInstall()
	cases := []struct{ name, page, label, want string }{
		{"rule form", rules.get(t, "/rules/edit"), "Review changes", "Checking…"},
		{"import", rules.get(t, "/rules/import"), "Review changes", "Checking…"},
		{"publish", review, "Publish", "Publishing…"},
		{"replace and publish", replacing, "Replace and publish", "Publishing…"},
		{"delete and publish", deleting, "Delete and publish", "Publishing…"},
		{"first rule, last step", first.get(t, "/welcome"), "Next", "Checking…"},
		{"first rule, publish", first.do(t, "POST", "/rules/edit", firstRule).Body.String(), "Publish", "Publishing…"},
		{"send request", twoRules(newHarness("requester")).get(t, "/requests/new?rule=ssm-prod"), "Submit request", "Sending…"},
		{"approve", req.get(t, "/requests?f=approve&open=ask2345678"), "Approve", "Approving…"},
		{"reject", req.get(t, "/requests/reject?id=ask2345678"), "Reject", "Rejecting…"},
		{"revoke", req.get(t, "/requests/revoke?id=mine234567"), "Revoke", "Revoking…"},
		{"cancel", req.get(t, "/requests/cancel?id=own2345678"), "Cancel request", "Cancelling…"},
	}
	for _, c := range cases {
		if got := busyOf(c.page, c.label); got != c.want {
			t.Errorf("%s: button %q has data-busy %q, want %q", c.name, c.label, got, c.want)
		}
	}
}

func TestActiveGrantNotYetReadySaysPreparing(t *testing.T) {
	h := newHarness("requester")
	f := h.app.flashOf(grant("new4567890", api.StateActive, me))
	if f.Text != "Preparing your access…" {
		t.Errorf("text %q", f.Text)
	}
	var b bytes.Buffer
	if err := parts.ExecuteTemplate(&b, "flash", f); err != nil {
		t.Fatal(err)
	}
	contains(t, b.String(), "Preparing your access…", `data-poll=`)
	lacks(t, b.String(), "Active", "Open in console")
}

func execPart(t *testing.T, name string, data any) string {
	t.Helper()
	var b bytes.Buffer
	if err := parts.ExecuteTemplate(&b, name, data); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestOnlyOutcomeNoticesCloseByThemselves(t *testing.T) {
	for _, kind := range []string{"success", "info"} {
		contains(t, execPart(t, "notice", notice{Kind: kind, Text: "done"}), ` data-autoclose`)
	}
	for _, kind := range []string{"error", "warning"} {
		lacks(t, execPart(t, "notice", notice{Kind: kind, Text: "done"}), "data-autoclose")
	}
	// A revocation still being followed turns into its outcome later: it stays.
	lacks(t, execPart(t, "notice", notice{Kind: "success", Text: "started", Follow: []string{"mine234567"}}), "data-autoclose")
	// The grant just requested keeps its console button for as long as the page is open.
	for _, kind := range []string{"info", "success", "error"} {
		g := grant("new4567890", api.StateActive, me)
		lacks(t, execPart(t, "flash", flash{Grant: g, Kind: kind, Ready: true, Console: "https://example.awsapps.com/start"}), "data-autoclose")
	}
}

func TestModalAccountKeepsNameAndId(t *testing.T) {
	h := newHarness("requester")
	named := grant("new2345678", "active", me, "revoke")
	named.AccountName = "prod"
	h.svc.lists["mine"] = []api.Grant{named}
	// The modal copies this cell's nodes: name and id are separate elements.
	contains(t, h.get(t, "/requests"), `<span>prod</span><span class="cell-sub">222222222222</span>`)

	js, err := files.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(js), "function grantTable")
	end := strings.Index(string(js)[start:], "\n}\n")
	body := string(js)[start : start+end]
	contains(t, body, "cloneNode(true)")
	lacks(t, body, `cellOf("Account")?.textContent`)
}
