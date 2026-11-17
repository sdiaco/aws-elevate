package ui

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

func TestEveryUserViewAnswers(t *testing.T) {
	h := newHarness("requester")
	for _, path := range []string{"/requests", "/requests/new", "/requests/new?rule=ssm-prod", "/history"} {
		out := h.get(t, path)
		contains(t, out, `class="nav"`, `aria-current="page"`)
	}
	if w := h.do(t, "GET", "/", nil); w.Code != 303 || w.Header().Get("Location") != "/requests" {
		t.Errorf("home: %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, gone := range []string{"/active", "/pending", "/request", "/problems"} {
		if w := h.do(t, "GET", gone, nil); w.Code != 404 {
			t.Errorf("%s still answers: %d", gone, w.Code)
		}
	}
}

func TestNavigationHasRequestsAndHistory(t *testing.T) {
	out := newHarness("requester").get(t, "/requests")
	contains(t, out, `href="/requests" aria-current="page"`, `href="/history"`, `user@example.com · requester`,
		`href="/requests/new"`)

	auditor := newHarness("auditor")
	out = auditor.get(t, "/requests")
	lacks(t, out, `href="/requests/new"`)
	if w := auditor.do(t, "GET", "/requests/new", nil); w.Code != 404 {
		t.Errorf("new request for an auditor: %d", w.Code)
	}
}

func TestServiceErrorsAreShownOnThePage(t *testing.T) {
	h := newHarness("requester")
	h.svc.grants = nil
	w := h.do(t, "GET", "/requests?open=abcdef2345", nil)
	contains(t, w.Body.String(), `class="alert error"`, "not_found: no grant abcdef2345", `class="nav"`)
}

// --- Requests ---

func requestsHarness(roles ...string) *harness {
	h := newHarness(roles...)
	mine := grant("mine234567", api.StateActive, me, "revoke")
	own := grant("own2345678", api.StatePending, me, "cancel")
	ask := grant("ask2345678", api.StatePending, other, "approve", "reject")
	exp := time.Now().Add(26 * time.Minute)
	ask.ApprovalExpiresAt = &exp
	ask.Privileged = true
	theirs := grant("their23456", api.StateActive, other, "revoke")
	theirs.RuleRemoved = true
	fail := failedRevoke("fail234567", "redrive")
	h.svc.lists["mine"] = []api.Grant{mine, own}
	h.svc.lists["approvable"] = []api.Grant{ask, theirs, mine}
	h.svc.lists["all"] = []api.Grant{ask, theirs, mine, own, fail}
	for _, g := range []api.Grant{mine, own, ask, theirs, fail} {
		h.svc.grants[g.ID] = g
	}
	return h
}

func scopes(h *harness) string {
	var s []string
	for _, q := range h.svc.queries {
		s = append(s, q.Scope)
	}
	return strings.Join(s, ",")
}

func TestARequesterSeesOnlyTheirRequestsWithoutFilters(t *testing.T) {
	h := requestsHarness("requester")
	out := h.get(t, "/requests")
	contains(t, out, `<span class="counter">(2)</span>`, "mine234567", "own2345678", `href="/requests/new"`)
	lacks(t, out, `class="segmented"`, "ask2345678", "their23456")
	if scopes(h) != "mine" || !h.svc.queries[0].Open {
		t.Errorf("queries %+v", h.svc.queries)
	}
}

func TestAnApproverFiltersAllMineAndToApprove(t *testing.T) {
	h := requestsHarness("requester", "approver")
	out := h.get(t, "/requests")
	contains(t, out,
		`aria-current="true">All (4)</a>`, `>Mine (2)</a>`, `>To approve (1)</a>`,
		`<span class="tag">privileged</span>`, `<span class="tag">rule removed</span>`,
	)
	lacks(t, out, "Failed (")
	if scopes(h) != "mine,approvable" {
		t.Errorf("scopes %s", scopes(h))
	}

	out = h.get(t, "/requests?f=approve")
	contains(t, out, `aria-current="true">To approve (1)</a>`, "ask2345678")
	lacks(t, out, "mine234567", "own2345678")
}

func TestAnAdminAlsoFiltersFailedRevocations(t *testing.T) {
	h := requestsHarness("admin")
	out := h.get(t, "/requests?f=failed")
	contains(t, out, `aria-current="true">Failed (1)</a>`, "fail234567", "Revoke failed")
	lacks(t, out, "ask2345678")
	if scopes(h) != "mine,all" {
		t.Errorf("scopes %s", scopes(h))
	}
	// A bad filter falls back to All.
	contains(t, h.get(t, "/requests?f=nope"), `aria-current="true">All (5)</a>`)
}

func TestRowsCarryOnlyTheActionsTheServiceGives(t *testing.T) {
	h := requestsHarness("requester", "approver")
	out := h.get(t, "/requests")
	// Only what is done on several rows sits in the header; every row can be selected to
	// open its panel.
	contains(t, out,
		`value="mine234567" data-select data-actions="revoke"`,
		`value="ask2345678" data-select aria-label`,
		`formaction="/requests/revoke" disabled data-action="revoke">Revoke…`,
	)
	lacks(t, out, `formaction="/requests/approve"`, `formaction="/requests/reject"`, `formaction="/requests/cancel"`, `formaction="/requests/retry"`)
}

func TestRowsInMotionPollTheirState(t *testing.T) {
	h := requestsHarness("requester", "approver")
	out := h.get(t, "/requests")
	contains(t, out, `data-poll="/requests/row/ask2345678?check=1" data-state="pending"`)
	lacks(t, out, `data-poll="/requests/row/mine234567`)

	out = h.get(t, "/requests/row/ask2345678?check=1")
	if !strings.HasPrefix(out, "<tr") {
		t.Fatalf("not a row: %s", out)
	}
	contains(t, out, `data-poll="/requests/row/ask2345678?check=1"`, `value="ask2345678" data-select`)
	h.svc.grants["ask2345678"] = grant("ask2345678", api.StateActive, other)
	out = h.get(t, "/requests/row/ask2345678?check=1")
	lacks(t, out, "data-poll")
	contains(t, out, `data-state="active"`, `value="ask2345678" data-select aria-label`)
}

func TestTheBottomPanelIsAlwaysThereAndFollowsTheChosenRow(t *testing.T) {
	h := requestsHarness("requester", "approver")
	out := h.get(t, "/requests?f=approve")
	contains(t, out,
		`<section class="split-panel" data-panel-slot`, "Select a request above",
		`href="/requests?f=approve&amp;open=ask2345678" data-panel="/panel/ask2345678?from=requests"`,
		`data-panel-resize`,
	)
	lacks(t, out, "get ask2345678")

	out = h.get(t, "/requests?f=approve&open=ask2345678")
	contains(t, out,
		`<tr data-row class="open"`,
		`href="/requests?f=approve" data-panel-close aria-label="Close"`,
		`data-tab="details"`, `data-tab="permissions"`,
		`data-tabpanel="permissions" data-load="/requests/permissions?rule=ssm-prod" hidden`,
		"incident 42", "leads",
		`formaction="/requests/approve"`, `href="/requests/reject?id=ask2345678"`,
	)
	lacks(t, out, "Open in console", "/requests/retry", "Timeline")

	// The fragment the page fetches when a row is chosen.
	out = h.get(t, "/panel/ask2345678?from=requests")
	if !strings.HasPrefix(out, `<section class="split-panel"`) {
		t.Fatalf("not a panel: %s", out)
	}
	contains(t, out, `href="/requests" data-panel-close`, "incident 42")
	lacks(t, out, `class="nav"`)
	contains(t, h.get(t, "/panel/ask2345678?from=history"), `href="/history" data-panel-close`)
	contains(t, h.get(t, "/panel/ask2345678?from=elsewhere"), `href="/requests" data-panel-close`)
	contains(t, h.get(t, "/panel/nope234567"), `class="alert error"`, "no grant nope234567")
}

func TestThePanelShowsWhenTheRequestWasApproved(t *testing.T) {
	h := requestsHarness("requester")
	g := grant("mine234567", api.StateActive, me, "revoke")
	g.Approver = &other
	h.svc.grants["mine234567"] = g
	at := time.Date(2026, 11, 15, 9, 3, 0, 0, time.Local)
	h.svc.timeline = []api.TimelineEntry{{At: at.Add(-3 * time.Minute), State: "pending"}, {At: at, State: "activating"}}
	contains(t, h.get(t, "/panel/mine234567"), "Approved by", "other@example.com · 2026-11-15 09:03:00")
}

func TestThePanelOfAFailedRevocationHasTheErrorTimeLeftAndRetry(t *testing.T) {
	out := requestsHarness("admin").get(t, "/requests?f=failed&open=fail234567")
	contains(t, out, "DeleteAccountAssignment: AccessDenied", "Still usable for", `<time data-until`,
		`formaction="/requests/retry"`)
}

func TestThePanelOfOwnActiveGrantOpensTheConsole(t *testing.T) {
	out := requestsHarness("requester").get(t, "/requests?open=mine234567")
	contains(t, out, `class="box access"`, `data-copy="aws elevate shell mine234567"`,
		`href="https://example.awsapps.com/start/#/console?account_id=222222222222" target="_blank" rel="noopener noreferrer">Open in console ↗`)
	out = requestsHarness("admin").get(t, "/requests?open=their23456")
	lacks(t, out, "Open in console", "aws elevate shell")
}

func TestApproveRejectAndCancelShowEachOutcome(t *testing.T) {
	h := requestsHarness("requester", "approver")
	h.svc.afterAction = api.StateActivating
	w := h.do(t, "POST", "/requests/approve", url.Values{"id": {"ask2345678"}})
	contains(t, w.Body.String(), "Results", "ask2345678", "Activating", `data-poll="/requests/outcome/ask2345678"`, `href="/requests"`)

	out := h.get(t, "/requests/reject?id=ask2345678")
	contains(t, out, `<form method="post" action="/requests/reject">`, `value="ask2345678"`, "Reject 1 request")
	h.svc.afterAction = api.StateClosed
	w = h.do(t, "POST", "/requests/reject", url.Values{"id": {"ask2345678"}})
	contains(t, w.Body.String(), "ask2345678", "Closed")

	contains(t, h.get(t, "/requests/cancel?id=own2345678"), `action="/requests/cancel"`, "Cancel 1 request")
	h.do(t, "POST", "/requests/cancel", url.Values{"id": {"own2345678"}})
	if got := strings.Join(h.svc.calls, ","); !strings.Contains(got, "approve ask2345678") ||
		!strings.Contains(got, "reject ask2345678") || !strings.Contains(got, "cancel own2345678") {
		t.Errorf("calls %v", h.svc.calls)
	}
}

func TestRevokeAsksForReasonAndShowsEachOutcome(t *testing.T) {
	h := requestsHarness("requester", "approver")
	out := h.get(t, "/requests/revoke?id=mine234567&id=their23456")
	contains(t, out, `<form method="post" action="/requests/revoke">`,
		`<input type="hidden" name="id" value="mine234567">`, `<input type="hidden" name="id" value="their23456">`,
		`<textarea name="reason"`, "Revoke 2 grants")

	w := h.do(t, "POST", "/requests/revoke", url.Values{"id": {"mine234567"}, "reason": {"  "}})
	contains(t, w.Body.String(), `class="alert error"`, "A reason is required")
	if len(h.svc.calls) != 0 {
		t.Fatalf("revoked without a reason: %v", h.svc.calls)
	}

	h.svc.afterAction = api.StateRevoking
	h.svc.revokeErr = map[string]error{"their23456": &api.Error{Status: 409, Code: "state_conflict", Message: "grant their23456 is closed"}}
	w = h.do(t, "POST", "/requests/revoke", url.Values{"id": {"mine234567", "their23456"}, "reason": {"done early"}})
	if strings.Join(h.svc.calls, ",") != "revoke done early mine234567,revoke done early their23456" {
		t.Errorf("calls %v", h.svc.calls)
	}
	contains(t, w.Body.String(), `data-poll="/requests/outcome/mine234567"`, "state_conflict: grant their23456 is closed")

	h.svc.grants["mine234567"] = grant("mine234567", api.StateRevoking, me)
	out = h.get(t, "/requests/outcome/mine234567")
	contains(t, out, `data-poll="/requests/outcome/mine234567"`)
	lacks(t, out, `class="check"`)
}

func TestRetryRunsTheFailedRevocationAgain(t *testing.T) {
	h := requestsHarness("admin")
	h.svc.afterAction = api.StateRevoking
	w := h.do(t, "POST", "/requests/retry", url.Values{"id": {"fail234567"}})
	contains(t, w.Body.String(), "fail234567", "Revoking")
	if h.svc.calls[len(h.svc.calls)-1] != "redrive fail234567" {
		t.Errorf("calls %v", h.svc.calls)
	}
	if w := newHarness("approver").do(t, "POST", "/requests/retry", url.Values{"id": {"fail234567"}}); w.Code != 404 {
		t.Errorf("retry for an approver: %d", w.Code)
	}
}

// --- New request ---

func TestNewRequestListsTheRulesAndShowsTheFormOfTheChosenOne(t *testing.T) {
	h := newHarness("requester")
	out := h.get(t, "/requests/new")
	contains(t, out, `<form method="get" action="/requests/new"`, `type="radio" name="rule" value="ssm-prod"`,
		"ssm-write", "Manual · leads", "4h")
	lacks(t, out, `<textarea`)
	// Admins and auditors see every rule elsewhere; here only what can be asked.
	if h.svc.calls[0] != "requestable rules" {
		t.Errorf("calls %v", h.svc.calls)
	}

	out = h.get(t, "/requests/new?rule=ssm-prod")
	contains(t, out,
		`id="rule-ssm-prod" checked`,
		`<form method="post" action="/requests/new" class="box">`,
		`<input type="hidden" name="rule" value="ssm-prod">`,
		`<option value="222222222222" selected>prod (222222222222)</option>`,
		`name="duration"`, `placeholder="1h"`, "Default 1h, at most 4h",
		`<textarea name="reason"`,
		`data-load="/requests/permissions?rule=ssm-prod"`,
		`href="/requests"`,
	)
	for _, c := range h.svc.calls {
		if strings.HasPrefix(c, "permissions") {
			t.Errorf("permissions read before opening: %v", h.svc.calls)
		}
	}
}

func TestChoosingARuleFetchesOnlyItsForm(t *testing.T) {
	h := newHarness("requester")
	contains(t, h.get(t, "/requests/new"), `data-swap="/requests/new/form?rule="`, `data-swap-into="request-form"`, `<div id="request-form">`)
	out := h.get(t, "/requests/new/form?rule=ssm-prod")
	if !strings.HasPrefix(out, `<form method="post" action="/requests/new"`) {
		t.Fatalf("not the form: %s", out)
	}
	contains(t, out, `<input type="hidden" name="rule" value="ssm-prod">`)
	lacks(t, out, `class="nav"`)
	if out := h.get(t, "/requests/new/form?rule=nope"); strings.TrimSpace(out) != "" {
		t.Errorf("form of an unknown rule: %s", out)
	}
}

func TestWhatYouWillGetIsAFragment(t *testing.T) {
	h := newHarness("requester")
	out := h.get(t, "/requests/permissions?rule=ssm-prod")
	contains(t, out, "AmazonSSMReadOnlyAccess", "ssm:PutParameter", "arn:aws:ssm:*:*:parameter/app/*", "&#34;Version&#34;: &#34;2012-10-17&#34;")
	lacks(t, out, `class="nav"`)

	h.svc.permsErr = &api.Error{Status: 404, Code: "not_found", Message: "permission set ssm-write no longer exists"}
	w := h.do(t, "GET", "/requests/permissions?rule=ssm-prod", nil)
	contains(t, w.Body.String(), `class="alert error"`, "permission set ssm-write no longer exists")
}

func TestSubmittingGoesBackToTheTableWithTheRequestFollowedOnTop(t *testing.T) {
	h := newHarness("requester")
	h.svc.grants["new4567890"] = grant("new4567890", api.StatePending, me, "cancel")
	w := h.do(t, "POST", "/requests/new", url.Values{"rule": {"ssm-prod"}, "account": {"222222222222"}, "duration": {"45m"}, "reason": {"incident 42"}})
	if w.Code != 303 || w.Header().Get("Location") != "/requests?sent=new4567890" {
		t.Fatalf("%d %s %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	want := api.CreateRequest{RuleID: "ssm-prod", Account: "222222222222", DurationSeconds: 2700, Reason: "incident 42"}
	if *h.svc.created != want {
		t.Errorf("created %+v", *h.svc.created)
	}

	out := h.get(t, "/requests?sent=new4567890")
	contains(t, out, `class="flash info"`, `data-poll="/requests/flash/new4567890"`, "Waiting for approval by leads", `class="spinner"`)
	lacks(t, out, "console?account_id")
}

func TestARefusedRequestKeepsTheForm(t *testing.T) {
	h := newHarness("requester")
	h.svc.createErr = &api.Error{Status: 400, Code: "invalid_request", Message: "duration over the rule maximum"}
	w := h.do(t, "POST", "/requests/new", url.Values{"rule": {"ssm-prod"}, "account": {"222222222222"}, "duration": {"9h"}, "reason": {"incident <42>"}})
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	contains(t, w.Body.String(), "duration over the rule maximum", `value="9h"`, ">incident &lt;42&gt;</textarea>")

	w = h.do(t, "POST", "/requests/new", url.Values{"rule": {"ssm-prod"}, "duration": {"soon"}, "reason": {"x"}})
	contains(t, w.Body.String(), `class="alert error"`, "like 45m or 2h")
}

func TestTheFlashWaitsForTheRoleBeforeTheConsole(t *testing.T) {
	h := newHarness("requester")
	h.svc.grants["new4567890"] = grant("new4567890", api.StateActive, me)
	// The page itself does not wait: the poll does.
	out := h.get(t, "/requests?sent=new4567890")
	contains(t, out, `data-poll=`, "Waiting for AWS to accept the role")
	if len(h.ready) != 0 {
		t.Errorf("waited on the page: %v", h.ready)
	}

	out = h.get(t, "/requests/flash/new4567890")
	if len(h.ready) != 1 {
		t.Fatalf("ready calls %v", h.ready)
	}
	contains(t, out, `class="flash success"`,
		"aws elevate shell new4567890", "export AWS_PROFILE=jit-ssm-write-222222222222",
		`<a class="btn primary" href="https://example.awsapps.com/start/#/console?account_id=222222222222" target="_blank" rel="noopener noreferrer">Open in console ↗</a>`,
	)
	lacks(t, out, "data-poll", `class="nav"`)
}

func TestTheFlashOfAClosedOrUnreadyRequest(t *testing.T) {
	h := newHarness("requester")
	g := grant("new4567890", api.StateClosed, me)
	reason, msg := "failed", "permission set ssm-write not found"
	g.CloseReason, g.Error = &reason, &msg
	h.svc.grants["new4567890"] = g
	out := h.get(t, "/requests/flash/new4567890")
	contains(t, out, `class="flash error"`, "failed", msg)
	lacks(t, out, "data-poll")

	h.app.Ready = func(context.Context, api.Grant) (string, error) { return "", errors.New("role not accepted after 60s") }
	h.svc.grants["new4567890"] = grant("new4567890", api.StateActive, me)
	out = h.get(t, "/requests/flash/new4567890")
	contains(t, out, `class="flash error"`, "role not accepted after 60s", "aws elevate shell new4567890")
}
