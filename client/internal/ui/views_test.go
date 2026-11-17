package ui

import (
	"context"
	"encoding/csv"
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
