package ui

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// open states: what the Active view lists (pending requests are in the Pending view).
var openStates = []string{api.StateActivating, api.StateActive, api.StateRevoking, api.StateRevokeFailed}

// openGrants are the caller's open grants and those they can see as approver or admin
// (design §11.2), each once.
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
			if !seen[g.ID] && slices.Contains(openStates, g.State) {
				seen[g.ID] = true
				out = append(out, g)
			}
		}
	}
	slices.SortStableFunc(out, func(x, y api.Grant) int { return expiry(x).Compare(expiry(y)) })
	return out, nil
}

func expiry(g api.Grant) time.Time {
	if g.ExpiresAt == nil {
		return time.Time{}
	}
	return *g.ExpiresAt
}

var activeFilters = []struct {
	key, label string
	keep       func(a *App, g api.Grant) bool
}{
	{"", "All", func(*App, api.Grant) bool { return true }},
	{"mine", "Mine", func(a *App, g api.Grant) bool { return a.mine(g) }},
	{"privileged", "Privileged", func(_ *App, g api.Grant) bool { return g.Privileged }},
	{"removed", "Rule removed", func(_ *App, g api.Grant) bool { return g.RuleRemoved }},
}

func (a *App) mine(g api.Grant) bool { return g.Requester.UserID == a.Me.User.UserID }

func (a *App) activePage(w http.ResponseWriter, r *http.Request) {
	grants, err := a.openGrants(r.Context())
	if err != nil {
		a.fail(w, "Active", "/active", err)
		return
	}
	key := r.URL.Query().Get("f")
	l := List{
		Title: "Active", Search: true, Empty: "No open grant.",
		Columns: []string{"State", "Requester", "Permission set", "Account", "Expires in", "Id", ""},
	}
	group := FilterGroup{}
	for _, f := range activeFilters {
		var kept []api.Grant
		for _, g := range grants {
			if f.keep(a, g) {
				kept = append(kept, g)
			}
		}
		path := "/active"
		if f.key != "" {
			path += "?f=" + f.key
		}
		group.Items = append(group.Items, Filter{Label: fmt.Sprintf("%s (%d)", f.label, len(kept)), Path: path, On: f.key == key})
		if f.key == key {
			for _, g := range kept {
				l.Rows = append(l.Rows, a.activeRowOf(g))
			}
		}
	}
	l.Filters = []FilterGroup{group}
	if slices.ContainsFunc(l.Rows, func(r Row) bool { return r.ID != "" }) {
		l.Actions = []Action{{Label: "Revoke…", Path: "/active/revoke", Danger: true}}
		// The fresh row must keep the selection column of this table.
		for i := range l.Rows {
			if l.Rows[i].Poll != "" {
				l.Rows[i].Poll += "?check=1"
			}
		}
	}
	a.show(w, http.StatusOK, "list", "Active", "/active", l)
}

func (a *App) activeRowOf(g api.Grant) Row {
	row := Row{Cells: []Cell{
		{State: g.State},
		{Text: g.Requester.UserName},
		{Text: g.SourcePs.Name, Tags: tags(g)},
		{Text: g.Account},
		{Until: expiry(g)},
		{Copy: g.ID},
		{},
	}}
	if can(g, "revoke") {
		row.ID = g.ID
	}
	if g.State == api.StateActivating || g.State == api.StateRevoking {
		row.Poll = "/active/row/" + g.ID
	}
	// Only the requester has the grant's role.
	if g.State == api.StateActive && a.mine(g) {
		row.Cells[6] = Cell{Text: "Open in console", Href: a.Console(g), External: true}
	}
	return row
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

// activeRow is one row of the Active list, fetched again while it is in transition.
func (a *App) activeRow(w http.ResponseWriter, r *http.Request) {
	a.freshRow(w, r, func(g api.Grant) Row { return a.activeRowOf(g) }, "?check=1")
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

func revokeTitle(ids []string) string {
	if len(ids) == 1 {
		return "Revoke 1 grant"
	}
	return fmt.Sprintf("Revoke %d grants", len(ids))
}

func (a *App) revokeConfirm(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query()["id"]
	if len(ids) == 0 {
		http.Redirect(w, r, "/active", http.StatusSeeOther)
		return
	}
	a.showRevoke(w, ids)
}

func (a *App) showRevoke(w http.ResponseWriter, ids []string, alerts ...Alert) {
	a.show(w, http.StatusOK, "confirm", revokeTitle(ids), "/active", confirm{
		Title: revokeTitle(ids), Text: "The access ends now. Each grant shows its own outcome.",
		Action: "/active/revoke", IDs: ids, Reason: true, Button: "Revoke", Back: "/active",
	}, alerts...)
}

func (a *App) revokeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, "Revoke", "/active", err)
		return
	}
	ids, reason := r.PostForm["id"], strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		a.showRevoke(w, ids, Alert{Kind: "error", Text: "A reason is required."})
		return
	}
	l := List{Title: "Revocation results", Columns: []string{"Outcome", "Requester", "Permission set", "Account", "Id"}}
	for _, id := range ids {
		g, err := a.Service.Revoke(r.Context(), id, reason)
		if err != nil {
			l.Rows = append(l.Rows, Row{Cells: []Cell{{Text: err.Error()}, {}, {}, {}, {Copy: id}}})
			continue
		}
		l.Rows = append(l.Rows, outcomeRowOf(*g))
	}
	a.show(w, http.StatusOK, "list", "Revocation results", "/active", l)
}

func outcomeRowOf(g api.Grant) Row {
	row := Row{Cells: []Cell{
		{State: g.State, Text: closeReason(g), Muted: true},
		{Text: g.Requester.UserName},
		{Text: g.SourcePs.Name},
		{Text: g.Account},
		{Copy: g.ID},
	}}
	if g.State == api.StateRevoking || g.State == api.StateActivating {
		row.Poll = "/active/outcome/" + g.ID
	}
	return row
}

// closeReason is the reason next to a closed state, " expired" or "".
func closeReason(g api.Grant) string {
	if g.CloseReason == nil {
		return ""
	}
	return " " + strings.ReplaceAll(*g.CloseReason, "_", " ")
}

func (a *App) outcomeRow(w http.ResponseWriter, r *http.Request) {
	a.freshRow(w, r, outcomeRowOf, "")
}
