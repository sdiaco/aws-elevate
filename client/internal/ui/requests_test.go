package ui

import (
	"context"
	"errors"
	"net/http/httptest"
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
	contains(t, out, `href="/requests" aria-current="page"`, `href="/history"`, `<span class="nav-user-name">user@example.com</span><span class="nav-user-roles">requester</span>`,
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
	contains(t, out, `<span class="counter">(2)</span>`, "mine234567", "own2345678", `href="/requests/new"`, ">Time left<")
	lacks(t, out, `class="segmented"`, "ask2345678", "their23456", ">Ends in<")
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

func TestAnAuditorSeesEveryOpenGrantWithoutActions(t *testing.T) {
	// An auditor only reads: every open grant, and no action button.
	h := requestsHarness("auditor")
	out := h.get(t, "/requests")
	contains(t, out, `<span class="counter">(5)</span>`, "ask2345678", "their23456", "fail234567")
	lacks(t, out, `/requests/revoke`, `href="/requests/new"`, `class="segmented"`)
	if scopes(h) != "mine,all" {
		t.Errorf("scopes %s", scopes(h))
	}
	// Also a requester: Revoke… for their own grants comes back.
	contains(t, requestsHarness("auditor", "requester").get(t, "/requests"), `/requests/revoke`)
}

func TestRowsCarryOnlyTheActionsTheServiceGives(t *testing.T) {
	h := requestsHarness("requester", "approver")
	out := h.get(t, "/requests")
	// What is done on several rows sits in the header's Actions menu, as EC2's Instance
	// state; the decisions on one request stay in its panel.
	contains(t, out,
		`value="mine234567" data-select data-actions="revoke"`,
		`value="own2345678" data-select data-actions="cancel"`,
		`value="ask2345678" data-select aria-label`,
		`<button type="button" class="btn" data-menu aria-haspopup="menu" aria-expanded="false">Actions</button>`,
		`role="menuitem" formaction="/requests/revoke" disabled data-action="revoke" data-confirm>Revoke…`,
		`role="menuitem" formaction="/requests/cancel" disabled data-action="cancel" data-confirm>Cancel request…`,
	)
	lacks(t, out, `formaction="/requests/approve"`, `formaction="/requests/reject"`, `formaction="/requests/retry"`)
}

func TestPendingRowsRefreshOnRequestAndMovingRowsPoll(t *testing.T) {
	h := requestsHarness("requester", "approver")
	out := h.get(t, "/requests")
	// A pending request waits for a person: a button reads it again, no timer does.
	contains(t, out, `data-refresh="/requests/row/ask2345678?check=1"`)
	lacks(t, out, `data-poll="/requests/row/ask2345678`, `data-poll="/requests/row/mine234567`)

	out = h.get(t, "/requests/row/ask2345678?check=1")
	if !strings.HasPrefix(out, "<tr") {
		t.Fatalf("not a row: %s", out)
	}
	contains(t, out, `value="ask2345678" data-select`)
	h.svc.grants["ask2345678"] = grant("ask2345678", api.StateActivating, other)
	out = h.get(t, "/requests/row/ask2345678?check=1")
	contains(t, out, `data-poll="/requests/row/ask2345678?check=1"`)
	lacks(t, out, "data-refresh")
	contains(t, out, `data-state="activating"`, `value="ask2345678" data-select aria-label`)
}

func TestAStartingRequestShowsStartingAndReadsItselfAgain(t *testing.T) {
	h := requestsHarness("requester", "approver")
	starting := grant("ask2345678", api.StatePending, other)
	starting.Starting = true
	h.svc.grants["ask2345678"] = starting
	out := h.get(t, "/requests/row/ask2345678")
	contains(t, out, `<span class="st st-starting">Starting</span>`, `data-poll="/requests/row/ask2345678"`, `data-state="starting"`)
	lacks(t, out, "data-refresh")

	h.svc.grants["ask2345678"] = grant("ask2345678", api.StatePending, other, "approve", "reject")
	out = h.get(t, "/requests/row/ask2345678")
	contains(t, out, `data-state="pending"`, `data-refresh="/requests/row/ask2345678?check=1"`)
	lacks(t, out, "data-poll")
}

func TestTheBottomPanelIsClosedUntilARowIsChosen(t *testing.T) {
	h := requestsHarness("requester", "approver")
	out := h.get(t, "/requests?f=approve")
	contains(t, out,
		`<section class="split-panel" data-panel-slot hidden></section>`,
		`href="/requests?f=approve&amp;open=ask2345678" data-panel="/panel/ask2345678?from=requests"`,
	)
	lacks(t, out, `data-panel-resize`)
	lacks(t, out, "get ask2345678")

	out = h.get(t, "/requests?f=approve&open=ask2345678")
	contains(t, out,
		`data-grant="ask2345678" class="open"`,
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
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skip("no time zone data")
	}
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = rome
	at := time.Date(2026, 11, 15, 9, 3, 0, 0, time.UTC)
	h.svc.timeline = []api.TimelineEntry{{At: at.Add(-3 * time.Minute), State: "pending"}, {At: at, State: "activating"}}
	// Local time with its zone, short; the full date on hover.
	contains(t, h.get(t, "/panel/mine234567"), "Approved by",
		`other@example.com · <time datetime="2026-11-15T09:03:00Z" title="2026-11-15 10:03:00 CET (UTC+01:00)">Nov 15, 10:03 CET</time>`)
}

func TestThePanelOfAFailedRevocationHasTheErrorTimeLeftAndRetry(t *testing.T) {
	out := requestsHarness("admin").get(t, "/requests?f=failed&open=fail234567")
	contains(t, out, "DeleteAccountAssignment: AccessDenied", "Still usable for", `<time data-until`,
		`formaction="/requests/retry"`)
}

func TestThePanelOfOwnActiveGrantOpensTheConsole(t *testing.T) {
	out := requestsHarness("requester").get(t, "/requests?open=mine234567")
	contains(t, out, `class="box access"`, `data-copy="aws elevate shell mine234567"`,
		`href="https://example.awsapps.com/start/#/console?account_id=222222222222" target="_blank" rel="noopener noreferrer">Open in console ↗`,
		// The portal opens as whoever is signed in to this browser: say so, and offer the
		// link for another browser profile.
		"Opens the AWS access portal as", "You will see No access", `aria-label="Copy link"`)
	// Once, in Access: not again in the panel header.
	if n := strings.Count(out, "Open in console"); n != 1 {
		t.Fatalf("Open in console %d times", n)
	}
	out = requestsHarness("admin").get(t, "/requests?open=their23456")
	lacks(t, out, "Open in console", "aws elevate shell")
}

// followed is the page a redirect after an action leads to.
func followed(t *testing.T, h *harness, w *httptest.ResponseRecorder, prefix string) string {
	t.Helper()
	loc := w.Header().Get("Location")
	if w.Code != 303 || !strings.HasPrefix(loc, prefix+"?notice=") {
		t.Fatalf("%d %q %s", w.Code, loc, w.Body)
	}
	out := h.get(t, loc)
	lacks(t, h.get(t, loc), `<div class="flash`) // the notice shows once
	return out
}

func TestApproveRejectAndCancelGoBackWithTheOutcome(t *testing.T) {
	h := requestsHarness("requester", "approver")
	h.svc.afterAction = api.StateActivating
	w := h.do(t, "POST", "/requests/approve", url.Values{"id": {"ask2345678"}})
	contains(t, followed(t, h, w, "/requests"), `class="flash success"`, "Request ask2345678: approved.", "data-dismiss")

	out := h.get(t, "/requests/reject?id=ask2345678")
	contains(t, out, `<form method="post" action="/requests/reject">`, `value="ask2345678"`, "Reject 1 request", `<textarea name="reason"`)
	w = h.do(t, "POST", "/requests/reject", url.Values{"id": {"ask2345678"}, "reason": {" "}})
	contains(t, w.Body.String(), `class="alert error"`, "A reason is required")
	h.svc.afterAction = api.StateClosed
	w = h.do(t, "POST", "/requests/reject", url.Values{"id": {"ask2345678"}, "reason": {"use the read-only rule"}})
	out = followed(t, h, w, "/requests")
	contains(t, out, "Request ask2345678: rejected.")
	lacks(t, out, "data-follow") // a rejection is final: nothing to follow

	contains(t, h.get(t, "/requests/cancel?id=own2345678"), `action="/requests/cancel"`, "Cancel 1 request",
		// The page is the fallback of a dialog: "Go back" leaves the view as it was.
		`data-close autofocus>Go back</a>`)
	contains(t, h.get(t, "/requests?open=own2345678"), `href="/requests/cancel?id=own2345678" data-confirm>Cancel request…`)
	h.do(t, "POST", "/requests/cancel", url.Values{"id": {"own2345678"}})
	if got := strings.Join(h.svc.calls, ","); !strings.Contains(got, "approve ask2345678") ||
		!strings.Contains(got, "reject use the read-only rule ask2345678") || !strings.Contains(got, "cancel own2345678") {
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
	contains(t, followed(t, h, w, "/requests"), `class="flash warning"`,
		"1 request: revocation started. 1 failed.", "their23456: state_conflict: grant their23456 is closed",
		// The page follows the started revocation and sums up its end.
		`data-follow="mine234567"`, `data-follow-failed="1"`)
}

func TestRetryRunsTheFailedRevocationAgain(t *testing.T) {
	h := requestsHarness("admin")
	h.svc.afterAction = api.StateRevoking
	w := h.do(t, "POST", "/requests/retry", url.Values{"id": {"fail234567"}})
	contains(t, followed(t, h, w, "/requests"), "Request fail234567: revocation retried.", `data-follow="fail234567"`)
	if h.svc.calls[len(h.svc.calls)-1] != "redrive fail234567" {
		t.Errorf("calls %v", h.svc.calls)
	}
	if w := newHarness("approver").do(t, "POST", "/requests/retry", url.Values{"id": {"fail234567"}}); w.Code != 404 {
		t.Errorf("retry for an approver: %d", w.Code)
	}
}

// --- New request ---

// twoRules gives the harness a second requestable rule, so none is chosen for the user.
func twoRules(h *harness) *harness {
	other := ruleSSM()
	other.ID = "ssm-dev"
	h.svc.rules = append(h.svc.rules, other)
	return h
}

func TestNewRequestListsTheRulesAndShowsTheFormOfTheChosenOne(t *testing.T) {
	h := twoRules(newHarness("requester"))
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
		`<form method="post" action="/requests/new" class="box launch-details" id="request">`,
		`<button class="btn primary" data-busy="Sending…" form="request">Submit request</button>`,
		`data-paginate="8"`, `data-filter`, `data-pager`,
		`<input type="hidden" name="rule" value="ssm-prod">`,
		`class="cards" data-limit="10"`, `data-more`,
		`<input type="radio" name="account" value="222222222222" checked required data-error="Choose an account.">`,
		`class="card-name">prod</span>`, `class="copy card-id" data-copy="222222222222"`,
		`name="duration"`, `placeholder="1h"`, "Default 1h, at most 4h",
		`<dd data-mirror="account" data-of="request">prod 222222222222</dd>`,
		`<dd data-mirror="duration" data-of="request" data-empty="1h (default)">1h (default)</dd>`,
		`<dd data-mirror="reason" data-of="request" class="clamp">-</dd>`,
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
	contains(t, h.get(t, "/requests/new"), `data-swap="/requests/new/form?rule="`, `data-swap-into="request-form"`, `<div id="request-form" class="launch-slot">`)
	out := h.get(t, "/requests/new/form?rule=ssm-prod")
	if !strings.HasPrefix(out, `<form method="post" action="/requests/new"`) {
		t.Fatalf("not the form: %s", out)
	}
	contains(t, out, `<input type="hidden" name="rule" value="ssm-prod">`, `<textarea name="reason" id="reason" rows="3" required data-keep>`)
	lacks(t, out, `class="nav"`)
	out = h.get(t, "/requests/new/form?rule=nope")
	contains(t, out, "Choose a rule to see its details", `<button class="btn primary" disabled>`)
	lacks(t, out, "<form")
}

func TestWhatYouWillGetIsAFragment(t *testing.T) {
	h := newHarness("requester")
	out := h.get(t, "/requests/permissions?rule=ssm-prod")
	contains(t, out, "AmazonSSMReadOnlyAccess", "ssm:PutParameter", "arn:aws:ssm:*:*:parameter/app/*", "&#34;Version&#34;: &#34;2012-10-17&#34;")
	// Managed policies are a table like the inline one, linked to the AWS reference.
	contains(t, out, "<th>Policy</th><th>Type</th>", "<td>AWS managed</td>",
		`href="https://docs.aws.amazon.com/aws-managed-policy/latest/reference/AmazonSSMReadOnlyAccess.html"`)
	lacks(t, out, `class="nav"`)

	h.svc.permsErr = &api.Error{Status: 404, Code: "not_found", Message: "permission set ssm-write no longer exists"}
	w := h.do(t, "GET", "/requests/permissions?rule=ssm-prod", nil)
	contains(t, w.Body.String(), `class="alert error"`, "permission set ssm-write no longer exists")
}

func TestSubmittingGoesBackToTheTableWithTheRequestFollowedOnTop(t *testing.T) {
	h := newHarness("requester")
	sent := grant("new4567890", api.StatePending, me, "cancel")
	exp := time.Now().Add(30 * time.Minute)
	sent.ApprovalExpiresAt = &exp
	h.svc.grants["new4567890"] = sent
	w := h.do(t, "POST", "/requests/new", url.Values{"rule": {"ssm-prod"}, "account": {"222222222222"}, "duration": {"45m"}, "reason": {"incident 42"}})
	if w.Code != 303 || w.Header().Get("Location") != "/requests?sent=new4567890" {
		t.Fatalf("%d %s %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	want := api.CreateRequest{RuleID: "ssm-prod", Account: "222222222222", DurationSeconds: 2700, Reason: "incident 42"}
	if *h.svc.created != want {
		t.Errorf("created %+v", *h.svc.created)
	}

	out := h.get(t, "/requests?sent=new4567890")
	// Sent, and nothing more to follow here: the row in the table tracks it.
	contains(t, out, `class="flash success"`, "<strong>Request new4567890</strong> submitted. An approver in leads must approve it within", "data-dismiss")
	lacks(t, out, `data-poll="/requests/flash/`, `data-refresh="/requests/flash/`, `class="spinner"`)
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
	contains(t, out, `data-poll=`, "Preparing your access…")
	lacks(t, out, "aws elevate shell new4567890")
	if len(h.ready) != 0 {
		t.Errorf("waited on the page: %v", h.ready)
	}

	// The first poll that finds it active says so at once; the wait is the next poll.
	out = h.get(t, "/requests/flash/new4567890")
	contains(t, out, `data-poll="/requests/flash/new4567890?ready=1"`, "Preparing your access…")
	lacks(t, out, "Activating", "aws elevate shell")
	if len(h.ready) != 0 {
		t.Fatalf("waited on the first active poll: %v", h.ready)
	}

	out = h.get(t, "/requests/flash/new4567890?ready=1")
	if len(h.ready) != 1 {
		t.Fatalf("ready calls %v", h.ready)
	}
	contains(t, out, `class="flash success"`,
		"aws elevate shell new4567890", `data-copy="--profile jit-ssm-write-222222222222"`,
		`<a class="btn primary" href="https://example.awsapps.com/start/#/console?account_id=222222222222" target="_blank" rel="noopener noreferrer">Open in console ↗</a>`,
	)
	lacks(t, out, "data-poll", `class="nav"`, "AWS_PROFILE")
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
	out = h.get(t, "/requests/flash/new4567890?ready=1")
	contains(t, out, `class="flash error"`, "role not accepted after 60s", "aws elevate shell new4567890")
}

func TestTheSelectionColumnIsThereEvenWithNothingToRevoke(t *testing.T) {
	h := newHarness("requester")
	h.svc.lists["mine"] = []api.Grant{grant("own2345678", api.StatePending, me, "cancel")}
	out := h.get(t, "/requests")
	contains(t, out, `data-select-all`, `value="own2345678" data-select`,
		`formaction="/requests/revoke" disabled data-action="revoke" data-confirm>Revoke…`)
}

func TestARequestNobodyApprovedInTimeSaysSo(t *testing.T) {
	g := grant("old2345678", api.StateClosed, me)
	reason := "timed_out"
	g.CloseReason = &reason
	if got := outcome(g); !strings.Contains(got, "not approved in time") {
		t.Errorf("outcome %q", got)
	}
}

func TestThePanelOfOwnActiveGrantLoadsTheProfileAfterShowing(t *testing.T) {
	h := requestsHarness("requester")
	out := h.get(t, "/requests?open=mine234567")
	contains(t, out, `data-fill="/requests/profile/mine234567"`, "Preparing the CLI profile")
	if len(h.ready) != 0 {
		t.Fatalf("the panel waited for AWS: %v", h.ready)
	}
	contains(t, h.get(t, "/requests/profile/mine234567"), `data-copy="--profile `)
	contains(t, h.get(t, "/requests/profile/their23456"), `class="alert error"`, "not an active grant of yours")
}

func TestAnActiveRowReadsItselfOnceItEndsAndAClosedOneStaysGrey(t *testing.T) {
	h := requestsHarness("requester", "approver")
	h.svc.grants["mine234567"] = grant("mine234567", api.StateActive, me, "revoke")
	out := h.get(t, "/requests/row/mine234567")
	// No timer while it is active: one read when its countdown reaches zero.
	contains(t, out, `data-expire="/requests/row/mine234567"`, `<time data-until`)
	lacks(t, out, "data-poll")
	// With the selection column, the row read again keeps it.
	h.svc.lists["mine"] = []api.Grant{h.svc.grants["mine234567"]}
	contains(t, h.get(t, "/requests"), `data-expire="/requests/row/mine234567?check=1"`)
	contains(t, h.get(t, "/requests/row/mine234567?check=1"), `data-expire="/requests/row/mine234567?check=1"`)

	h.svc.grants["mine234567"] = grant("mine234567", api.StateClosed, me)
	out = h.get(t, "/requests/row/mine234567")
	contains(t, out, `data-state="closed"`, `<span class="st st-closed">Closed</span>`)
	lacks(t, out, "data-expire", "data-poll", "<time data-until", `data-actions="revoke"`)
}

func TestEachRequestsFilterSaysWhatIsEmpty(t *testing.T) {
	h := newHarness("admin", "approver")
	contains(t, h.get(t, "/requests?f=failed"), "No failed grants.")
	contains(t, h.get(t, "/requests?f=approve"), "Nothing to approve.")
	// Who cannot request has no "Mine": it would always count 0.
	lacks(t, h.get(t, "/requests"), ">Mine (")
	contains(t, newHarness("requester", "approver").get(t, "/requests?f=mine"), ">Mine (", "You have no open request.")
	contains(t, newHarness("requester").get(t, "/requests"), "You have no open request.")
	contains(t, newHarness("auditor").get(t, "/requests"), "No open request.")
}

func TestAPendingRowSaysWithinWhenToApprove(t *testing.T) {
	h := requestsHarness("requester")
	out := h.get(t, "/requests")
	contains(t, out, `<span class="muted">Approve within </span><time`)
}

func TestTheOnlyRequestableRuleIsAlreadyChosen(t *testing.T) {
	out := newHarness("requester").get(t, "/requests/new")
	contains(t, out, `value="ssm-prod" id="rule-ssm-prod" checked`, `<textarea name="reason"`)
	// With two or more, the user chooses: nothing is picked for them.
	lacks(t, twoRules(newHarness("requester")).get(t, "/requests/new"), `<textarea name="reason"`)
}

// An action from the panel goes back to the filter it was taken from, read from the
// page that sent it; anything but a known filter goes back to all requests.
func TestAnActionGoesBackToTheFilterItCameFrom(t *testing.T) {
	h := requestsHarness("requester", "approver")
	h.svc.afterAction = api.StateActivating
	for referer, want := range map[string]string{
		"http://127.0.0.1:1/requests?f=approve&open=ask2345678": "/requests?f=approve&notice=",
		"http://127.0.0.1:1/requests?f=evil":                    "/requests?notice=",
		"http://127.0.0.1:1/history?f=approve":                  "/requests?notice=",
		"":                                                      "/requests?notice=",
	} {
		r := httptest.NewRequest("POST", "/requests/approve", strings.NewReader("id=ask2345678"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Referer", referer)
		w := httptest.NewRecorder()
		Pages(h.app).ServeHTTP(w, r)
		if got := w.Header().Get("Location"); !strings.HasPrefix(got, want) {
			t.Errorf("from %q: went to %q, want %s…", referer, got, want)
		}
	}
}

func TestAnAccountWithAnOpenGrantCannotBeChosenAgain(t *testing.T) {
	h := newHarness("requester")
	rule := h.svc.rules[0]
	rule.Accounts = append(rule.Accounts, api.Ref{ID: "333333333333", Name: "stage"})
	h.svc.rules[0] = rule
	h.svc.openGrants = []api.OpenGrant{{RuleID: rule.ID, Account: "333333333333", GrantID: "abcde23456", State: "pending"}}
	out := h.get(t, "/requests/new?rule="+rule.ID)
	contains(t, out,
		`<input type="radio" name="account" value="222222222222" required data-error="Choose an account.">`,
		`<input type="radio" name="account" value="333333333333" disabled>`,
		`Pending · <a href="/requests?open=abcde23456">abcde23456</a>`, "waiting for approval",
	)
}

func TestARuleOpenOnEveryAccountGoesLastAndGreyed(t *testing.T) {
	h := twoRules(newHarness("requester"))
	h.svc.openGrants = []api.OpenGrant{{RuleID: "ssm-prod", Account: "222222222222", GrantID: "abcde23456", State: "active"}}
	out := h.get(t, "/requests/new")
	contains(t, out,
		`<tr class="covered">`, `id="rule-ssm-prod" disabled`,
		`Active · <a href="/requests?open=abcde23456">abcde23456</a>`, "you already have this access",
	)
	if strings.Index(out, `value="ssm-dev"`) > strings.Index(out, `value="ssm-prod"`) {
		t.Error("the covered rule is not last")
	}
	// The only rule left to ask for is not chosen for the user when it is covered.
	h = newHarness("requester")
	h.svc.openGrants = []api.OpenGrant{{RuleID: "ssm-prod", Account: "222222222222", GrantID: "abcde23456", State: "active"}}
	lacks(t, h.get(t, "/requests/new"), `id="request"`)
}
