package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// The Requests view (design §11.2): one table of the open grants, a bottom panel for the
// detail of one, and the actions the service gives on the selected rows.

// requestFilter is one quick filter of the table; who sees which follows the role.
type requestFilter struct {
	key, label string
	keep       func(a *App, g api.Grant) bool
}

func (a *App) requestFilters() []requestFilter {
	if !a.has("approver") && !a.has("admin") {
		return nil // a requester sees only their own requests, without filters
	}
	f := []requestFilter{
		{"", "All", func(*App, api.Grant) bool { return true }},
		{"mine", "Mine", func(a *App, g api.Grant) bool { return a.mine(g) }},
		{"approve", "To approve", func(_ *App, g api.Grant) bool {
			return g.State == api.StatePending && (can(g, "approve") || can(g, "reject"))
		}},
	}
	if a.has("admin") {
		f = append(f, requestFilter{"failed", "Failed", func(_ *App, g api.Grant) bool { return g.State == api.StateRevokeFailed }})
	}
	return f
}

func (a *App) mine(g api.Grant) bool { return g.Requester.UserID == a.me().User.UserID }

// openGrants are the caller's open grants and those they see as approver or admin, each
// once, the soonest to end first.
func (a *App) openGrants(ctx context.Context) ([]api.Grant, error) {
	queries := []api.ListQuery{{Scope: "mine", Open: true}}
	switch {
	case a.has("admin"):
		queries = append(queries, api.ListQuery{Scope: "all", Open: true})
	case a.has("approver"):
		queries = append(queries, api.ListQuery{Scope: "approvable"})
	}
	var out []api.Grant
	seen := map[string]bool{}
	for _, q := range queries {
		grants, err := listAll(ctx, a.Service, q)
		if err != nil {
			return nil, err
		}
		for _, g := range grants {
			if !seen[g.ID] && g.State != api.StateClosed {
				seen[g.ID] = true
				out = append(out, g)
			}
		}
	}
	slices.SortStableFunc(out, func(x, y api.Grant) int { return ends(x).Compare(ends(y)) })
	return out, nil
}

// ends is when a request stops waiting for an answer, or when a grant expires.
func ends(g api.Grant) time.Time {
	switch {
	case g.State == api.StatePending && g.ApprovalExpiresAt != nil:
		return *g.ApprovalExpiresAt
	case g.ExpiresAt != nil:
		return *g.ExpiresAt
	}
	return time.Time{}
}

// requestActions are the header buttons: only what is done on several rows at once. The
// decisions on one request (approve, reject, cancel, retry) are in the panel, next to its
// detail (design §11.2). Each is enabled only when every selected row allows it.
var requestActions = []Action{
	{Label: "Revoke…", Path: "/requests/revoke", Name: "revoke", Danger: true},
}

// listView is a page of one list and its bottom panel: Requests, History, Rules and
// Config versions.
type listView struct {
	List   List
	Flash  *flash
	Notice *notice
	Panel  panelSlot
	Alert  *Alert
}

// panelSlot is the bottom panel of a page, closed until a row is chosen (design §11.1):
// the detail of a grant, a rule or a rules version.
type panelSlot struct {
	Panel   *panel
	Rule    *rulePanel
	Version *versionPanel
}

func (a *App) requestsPage(w http.ResponseWriter, r *http.Request) {
	grants, err := a.openGrants(r.Context())
	if err != nil {
		a.fail(w, "Requests", "/requests", err)
		return
	}
	filters := a.requestFilters()
	key := r.URL.Query().Get("f")
	if !slices.ContainsFunc(filters, func(f requestFilter) bool { return f.key == key }) {
		key = ""
	}
	here := "/requests"
	if key != "" {
		here += "?f=" + key
	}
	l := List{
		Title: "Requests", Search: true, Empty: "No open request.",
		Columns: []string{"Id", "State", "Requester", "Permission set", "Account", "Ends in"},
	}
	if a.has("requester") {
		l.Tools = []Action{{Label: "New request", Path: "/requests/new", Primary: true}}
	}
	kept := grants
	if filters != nil {
		group := FilterGroup{}
		for _, f := range filters {
			var in []api.Grant
			for _, g := range grants {
				if f.keep(a, g) {
					in = append(in, g)
				}
			}
			path := "/requests"
			if f.key != "" {
				path += "?f=" + f.key
			}
			group.Items = append(group.Items, Filter{Label: fmt.Sprintf("%s (%d)", f.label, len(in)), Path: path, On: f.key == key})
			if f.key == key {
				kept = in
			}
		}
		l.Filters = []FilterGroup{group}
	}
	for _, g := range kept {
		l.Rows = append(l.Rows, a.requestRowOf(g, here))
	}
	// The selection column is always there, as in the console: Revoke stays disabled until
	// the selected rows are all ones the service lets this user revoke.
	l.Actions = requestActions
	for i := range l.Rows {
		if l.Rows[i].Poll != "" {
			l.Rows[i].Poll += "?check=1"
		}
	}
	v := listView{List: l, Notice: a.noticeOf(r)}
	if id := r.URL.Query().Get("sent"); id != "" {
		if d, err := a.Service.Grant(r.Context(), id); err != nil {
			v.Alert = &Alert{Kind: "error", Text: err.Error()}
		} else {
			f := a.flashOf(d.Grant)
			v.Flash = &f
		}
	}
	// The detail is read only when opened (design §11.1).
	if id := r.URL.Query().Get("open"); id != "" {
		p, err := a.panel(r.Context(), id, here)
		if err != nil {
			a.fail(w, "Requests", "/requests", err)
			return
		}
		v.Panel.Panel = p
		markOpen(v.List.Rows, id)
	}
	a.show(w, http.StatusOK, "listpanel", "Requests", "/requests", v)
}

// requestRowOf is one row; its id opens the panel on the page at here.
func (a *App) requestRowOf(g api.Grant, here string) Row {
	row := Row{Key: g.ID, State: g.State, Cells: []Cell{
		{Text: g.ID, Href: withParam(here, "open", g.ID), Panel: "/panel/" + g.ID + "?from=requests"},
		{State: g.State, Refresh: refreshOf(g)},
		{Text: g.Requester.UserName},
		{Text: g.SourcePs.Name, Tags: tags(g)},
		{Text: g.Account},
		{Until: ends(g)},
	}}
	for _, act := range requestActions {
		if can(g, act.Name) {
			row.Actions = append(row.Actions, act.Name)
		}
	}
	// Every row can be selected: a single selection opens its panel, as in the console.
	row.ID = g.ID
	if inMotion(g.State) {
		row.Poll = "/requests/row/" + g.ID
	}
	return row
}

// refreshOf is the address a pending row reads itself again from, on request.
func refreshOf(g api.Grant) string {
	if g.State != api.StatePending {
		return ""
	}
	return "/requests/row/" + g.ID + "?check=1"
}

// inMotion states change within seconds without anyone on this page acting: the page asks
// for them. A pending request waits for a person, up to 30 minutes: its row has a refresh
// button instead.
func inMotion(state string) bool {
	return state == api.StateActivating || state == api.StateRevoking
}

func withParam(path, key, value string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + key + "=" + url.QueryEscape(value)
}

func tags(g api.Grant) []string {
	var t []string
	if g.Privileged {
		t = append(t, "privileged")
	}
	if g.RuleRemoved {
		t = append(t, "rule removed")
	}
	return t
}

// requestRow is one row fetched again while its state can change. The id link opens
// the panel on the table's first filter: the row does not know the page around it.
func (a *App) requestRow(w http.ResponseWriter, r *http.Request) {
	a.freshRow(w, r, func(g api.Grant) Row { return a.requestRowOf(g, "/requests") }, "?check=1")
}

func (a *App) freshRow(w http.ResponseWriter, r *http.Request, rowOf func(api.Grant) Row, checkSuffix string) {
	d, err := a.Service.Grant(r.Context(), r.PathValue("id"))
	if err != nil {
		partError(w, err)
		return
	}
	row := rowOf(d.Grant)
	check := r.URL.Query().Get("check") == "1"
	if check && row.Poll != "" {
		row.Poll += checkSuffix
	}
	part(w, "row", rowCtx{Row: row, Check: check})
}

// panel is the detail of one grant at the bottom of the page, shared with History.
type panel struct {
	Grant      api.Grant
	ApprovedAt time.Time
	Close      string
	Console    string
}

func (a *App) panel(ctx context.Context, id, closeTo string) (*panel, error) {
	d, err := a.Service.Grant(ctx, id)
	if err != nil {
		return nil, err
	}
	p := &panel{Grant: d.Grant, Close: closeTo, ApprovedAt: approvedAt(*d)}
	// Only the requester has the grant's role.
	if a.mine(d.Grant) && d.Grant.State == api.StateActive {
		p.Console = a.Console(d.Grant)
	}
	return p, nil
}

// profilePart writes the CLI profile of the user's own active grant and shows its name, for
// the Access box of the panel: read after the panel shows, since AWS may need a few seconds
// to accept a role that just became active (spike C12).
func (a *App) profilePart(w http.ResponseWriter, r *http.Request) {
	d, err := a.Service.Grant(r.Context(), r.PathValue("id"))
	if err != nil {
		partError(w, err)
		return
	}
	if !a.mine(d.Grant) || d.Grant.State != api.StateActive {
		partError(w, fmt.Errorf("grant %s is not an active grant of yours", d.Grant.ID))
		return
	}
	profile, err := a.Ready(r.Context(), d.Grant)
	if err != nil {
		partError(w, err)
		return
	}
	part(w, "profile-command", profile)
}

// approvedAt is when an approver let the request go on: the grant has no field for it,
// its timeline has the step out of pending.
func approvedAt(d api.GrantDetail) time.Time {
	if d.Grant.Approver == nil {
		return time.Time{}
	}
	for _, e := range d.Timeline {
		if e.State == api.StateActivating {
			return e.At
		}
	}
	return time.Time{}
}

// panelPages are the pages a panel fragment can belong to, and where its close link goes
// without the script.
var panelPages = map[string]string{"requests": "/requests", "history": "/history"}

// panelPart is the panel of one grant alone, fetched when a row is chosen.
func (a *App) panelPart(w http.ResponseWriter, r *http.Request) {
	closeTo, ok := panelPages[r.URL.Query().Get("from")]
	if !ok {
		closeTo = "/requests"
	}
	p, err := a.panel(r.Context(), r.PathValue("id"), closeTo)
	if err != nil {
		partError(w, err)
		return
	}
	part(w, "panel", panelSlot{Panel: p})
}

// markOpen highlights the row whose panel is open.
func markOpen(rows []Row, id string) {
	for i := range rows {
		rows[i].Open = rows[i].Key == id
	}
}

// confirm asks before an action on grants: it lists the ids and, for a revocation, asks
// for the reason (design §11.1).
type confirm struct {
	Title  string
	Text   string
	Action string
	IDs    []string
	Reason bool
	Button string
	Back   string
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (a *App) revokeConfirm(w http.ResponseWriter, r *http.Request) {
	a.showConfirm(w, r, r.URL.Query()["id"], "/requests/revoke")
}

func (a *App) rejectConfirm(w http.ResponseWriter, r *http.Request) {
	a.showConfirm(w, r, r.URL.Query()["id"], "/requests/reject")
}

func (a *App) cancelConfirm(w http.ResponseWriter, r *http.Request) {
	a.showConfirm(w, r, r.URL.Query()["id"], "/requests/cancel")
}

func (a *App) showConfirm(w http.ResponseWriter, r *http.Request, ids []string, action string, alerts ...Alert) {
	if len(ids) == 0 {
		http.Redirect(w, r, "/requests", http.StatusSeeOther)
		return
	}
	c := confirm{Action: action, IDs: ids, Back: "/requests"}
	switch action {
	case "/requests/revoke":
		c.Title, c.Button, c.Reason = "Revoke "+count(len(ids), "grant", "grants"), "Revoke", true
		c.Text = "The access ends now. Each grant shows its own outcome."
	case "/requests/reject":
		c.Title, c.Button, c.Reason = "Reject "+count(len(ids), "request", "requests"), "Reject", true
		c.Text = "The requester reads the reason in the request details and in the history."
	case "/requests/cancel":
		c.Title, c.Button = "Cancel "+count(len(ids), "request", "requests"), "Cancel request"
		c.Text = "The request stops waiting for an approval."
	}
	a.show(w, http.StatusOK, "confirm", c.Title, "/requests", c, alerts...)
}

func (a *App) revoke(w http.ResponseWriter, r *http.Request) {
	a.withReason(w, r, "/requests/revoke", "revocation started", a.Service.Revoke)
}

// withReason runs an action that needs a reason, or shows the confirmation again when
// the reason is empty.
func (a *App) withReason(w http.ResponseWriter, r *http.Request, action, done string, act func(ctx context.Context, id, reason string) (*api.Grant, error)) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, "Requests", "/requests", err)
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		a.showConfirm(w, r, r.PostForm["id"], action, Alert{Kind: "error", Text: "A reason is required."})
		return
	}
	a.results(w, r, done, func(ctx context.Context, id string) (*api.Grant, error) {
		return act(ctx, id, reason)
	})
}

func (a *App) approve(w http.ResponseWriter, r *http.Request) {
	a.results(w, r, "approved", a.Service.Approve)
}
func (a *App) reject(w http.ResponseWriter, r *http.Request) {
	a.withReason(w, r, "/requests/reject", "rejected", a.Service.Reject)
}
func (a *App) cancel(w http.ResponseWriter, r *http.Request) {
	a.results(w, r, "cancelled", a.Service.Cancel)
}
func (a *App) retry(w http.ResponseWriter, r *http.Request) {
	a.results(w, r, "revocation retried", a.Service.Redrive)
}

// results runs one action on each selected grant and goes back to Requests, which shows
// the outcome at its top: one line, plus one line per grant the service refused. The rows
// show each grant's new state.
func (a *App) results(w http.ResponseWriter, r *http.Request, done string, act func(ctx context.Context, id string) (*api.Grant, error)) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, "Requests", "/requests", err)
		return
	}
	ids := r.PostForm["id"]
	var ok []string
	var failed []string
	for _, id := range ids {
		if _, err := act(r.Context(), id); err != nil {
			failed = append(failed, id+": "+err.Error())
			continue
		}
		ok = append(ok, id)
	}
	a.redirectWithNotice(w, r, "/requests", outcomeNotice(ok, failed, done))
}

// outcomeNotice sums up an action on some grants.
func outcomeNotice(ok, failed []string, done string) notice {
	switch {
	case len(failed) == 0 && len(ok) == 1:
		return notice{Kind: "success", Text: "Request " + ok[0] + ": " + done + "."}
	case len(failed) == 0:
		return notice{Kind: "success", Text: fmt.Sprintf("%s: %s.", count(len(ok), "request", "requests"), done)}
	case len(ok) == 0 && len(failed) == 1:
		return notice{Kind: "error", Text: "Request " + failed[0]}
	case len(ok) == 0:
		return notice{Kind: "error", Text: count(len(failed), "request", "requests") + " failed.", Items: failed}
	}
	return notice{
		Kind:  "warning",
		Text:  fmt.Sprintf("%s: %s. %d failed.", count(len(ok), "request", "requests"), done, len(failed)),
		Items: failed,
	}
}

// closeReason is the reason next to a closed state, " expired" or "".
func closeReason(g api.Grant) string {
	if g.CloseReason == nil {
		return ""
	}
	return " " + reasonLabel(*g.CloseReason)
}

// reasonLabel is a close reason in words. A request nobody approved in time says so: the
// bare "timed out" does not tell the requester what timed out.
func reasonLabel(reason string) string {
	if reason == "timed_out" {
		return "not approved in time"
	}
	return strings.ReplaceAll(reason, "_", " ")
}

func errorOf(g api.Grant) string {
	if g.Error == nil {
		return ""
	}
	return *g.Error
}
