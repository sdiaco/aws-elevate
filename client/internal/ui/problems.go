package ui

import (
	"fmt"
	"net/http"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// failedActivationsWindow bounds the read of closed grants: the history of every user is
// read page by page, so the Problems view looks back one week only.
const failedActivationsWindow = 7 * 24 * time.Hour

// problemsPage lists failed revocations, which may leave an access usable until its
// expiry, and failed activations (design §11.2). Retry is the service's redrive.
func (a *App) problemsPage(w http.ResponseWriter, r *http.Request) {
	activations := r.URL.Query().Get("f") == "activations"
	failedRevokes, err := a.failedRevocations(r)
	if err != nil {
		a.fail(w, "Problems", "/problems", err)
		return
	}
	failedActs, err := a.failedActivations(r)
	if err != nil {
		a.fail(w, "Problems", "/problems", err)
		return
	}
	l := List{Title: "Problems", Search: true, Filters: []FilterGroup{{Label: "Problem", Items: []Filter{
		{Label: fmt.Sprintf("Revocations failed (%d)", len(failedRevokes)), Path: "/problems", On: !activations},
		{Label: fmt.Sprintf("Activations failed, 7 days (%d)", len(failedActs)), Path: "/problems?f=activations", On: activations},
	}}}}
	if activations {
		l.Empty = "No activation failed in the last 7 days."
		l.Columns = []string{"State", "Requester", "Permission set", "Account", "Error", "When", "Id"}
		for _, g := range failedActs {
			l.Rows = append(l.Rows, Row{Cells: []Cell{
				{State: g.State, Text: closeReason(g), Muted: true}, {Text: g.Requester.UserName},
				{Text: g.SourcePs.Name, Tags: tags(g)}, {Text: g.Account}, {Text: errorOf(g)},
				{At: g.CreatedAt}, {Copy: g.ID},
			}})
		}
		a.show(w, http.StatusOK, "list", "Problems", "/problems", l)
		return
	}
	l.Empty = "No revocation failed."
	l.Columns = []string{"State", "Requester", "Permission set", "Account", "Error", "Still usable for", "Id"}
	for _, g := range failedRevokes {
		row := Row{Cells: []Cell{
			{State: g.State}, {Text: g.Requester.UserName}, {Text: g.SourcePs.Name, Tags: tags(g)},
			{Text: g.Account}, {Text: errorOf(g)}, {Until: expiry(g)}, {Copy: g.ID},
		}}
		if can(g, "redrive") {
			row.ID = g.ID
		}
		l.Rows = append(l.Rows, row)
	}
	if len(l.Rows) > 0 {
		l.Actions = []Action{{Label: "Retry", Path: "/problems/retry"}}
	}
	a.show(w, http.StatusOK, "list", "Problems", "/problems", l)
}

func (a *App) failedRevocations(r *http.Request) ([]api.Grant, error) {
	open, err := listAll(r.Context(), a.Service, api.ListQuery{Scope: "all", Open: true})
	if err != nil {
		return nil, err
	}
	var out []api.Grant
	for _, g := range open {
		if g.State == api.StateRevokeFailed {
			out = append(out, g)
		}
	}
	return out, nil
}

func (a *App) failedActivations(r *http.Request) ([]api.Grant, error) {
	from := time.Now().Add(-failedActivationsWindow).UTC().Format(time.RFC3339)
	recent, err := listAll(r.Context(), a.Service, api.ListQuery{Scope: "all", From: from})
	if err != nil {
		return nil, err
	}
	var out []api.Grant
	for _, g := range recent {
		if g.State == api.StateClosed && g.CloseReason != nil && *g.CloseReason == "failed" {
			out = append(out, g)
		}
	}
	return out, nil
}

func errorOf(g api.Grant) string {
	if g.Error == nil {
		return ""
	}
	return *g.Error
}

// retry asks the service to run each failed revocation again and shows each outcome.
func (a *App) retry(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, "Retry", "/problems", err)
		return
	}
	l := List{Title: "Retry results", Columns: []string{"Outcome", "Requester", "Permission set", "Account", "Id"}}
	for _, id := range r.PostForm["id"] {
		g, err := a.Service.Redrive(r.Context(), id)
		if err != nil {
			l.Rows = append(l.Rows, Row{Cells: []Cell{{Text: err.Error()}, {}, {}, {}, {Copy: id}}})
			continue
		}
		l.Rows = append(l.Rows, outcomeRowOf(*g))
	}
	a.show(w, http.StatusOK, "list", "Retry results", "/problems", l)
}
