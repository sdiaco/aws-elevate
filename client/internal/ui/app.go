package ui

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// Service is the part of the api client the views use. Every decision comes from it:
// the views show the actions each grant carries and nothing else.
type Service interface {
	RequestableRules(ctx context.Context) (*api.RuleList, error)
	Permissions(ctx context.Context, ruleID string) (*api.Permissions, error)
	CreateGrant(ctx context.Context, req api.CreateRequest) (*api.Grant, error)
	Grant(ctx context.Context, id string) (*api.GrantDetail, error)
	Grants(ctx context.Context, q api.ListQuery) (*api.GrantList, error)
	Approve(ctx context.Context, id string) (*api.Grant, error)
	Reject(ctx context.Context, id string) (*api.Grant, error)
	Cancel(ctx context.Context, id string) (*api.Grant, error)
	Revoke(ctx context.Context, id, reason string) (*api.Grant, error)
}

// App is what the views need from the process that holds the credentials.
type App struct {
	Me      api.Me
	Service Service
	// Ready waits until AWS accepts the grant's role, writes its CLI profile and returns
	// the profile name (spike C12: the role works 7–9 s after activation).
	Ready func(ctx context.Context, g api.Grant) (string, error)
	// Console is the address that opens the AWS console with the grant's role.
	Console func(g api.Grant) string
}

func (a *App) has(role string) bool { return slices.Contains(a.Me.Roles, role) }

// Pages are the views of the interface. A view the roles do not allow is not served.
func Pages(a *App) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticFiles())
	mux.HandleFunc("GET /{$}", a.home)
	if a.has("requester") {
		mux.HandleFunc("GET /request", a.requestPage)
		mux.HandleFunc("POST /request", a.requestSubmit)
	}
	mux.HandleFunc("GET /request/permissions", a.permissions)
	mux.HandleFunc("GET /request/{id}", a.progressPage)
	mux.HandleFunc("GET /request/{id}/progress", a.progressPart)
	mux.HandleFunc("GET /active", a.activePage)
	mux.HandleFunc("GET /active/row/{id}", a.activeRow)
	mux.HandleFunc("GET /active/revoke", a.revokeConfirm)
	mux.HandleFunc("POST /active/revoke", a.revokeSubmit)
	mux.HandleFunc("GET /active/outcome/{id}", a.outcomeRow)
	mux.HandleFunc("GET /pending", a.pendingPage)
	mux.HandleFunc("POST /pending/approve", a.approve)
	mux.HandleFunc("GET /pending/reject", a.rejectConfirm)
	mux.HandleFunc("POST /pending/reject", a.reject)
	mux.HandleFunc("GET /pending/cancel", a.cancelConfirm)
	mux.HandleFunc("POST /pending/cancel", a.cancel)
	mux.HandleFunc("GET /pending/history", a.requesterHistory)
	mux.HandleFunc("GET /history", a.historyPage)
	mux.HandleFunc("GET /history/export.csv", a.historyExport)
	return mux
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	to := "/active"
	if a.has("requester") {
		to = "/request"
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (a *App) nav() []NavGroup {
	var items []NavItem
	if a.has("requester") {
		items = append(items, NavItem{Label: "Request", Path: "/request"})
	}
	items = append(items,
		NavItem{Label: "Active", Path: "/active"},
		NavItem{Label: "Pending", Path: "/pending"},
		NavItem{Label: "History", Path: "/history"},
	)
	return []NavGroup{{Items: items}}
}

// show renders a view inside the base layout.
func (a *App) show(w http.ResponseWriter, status int, view, title, path string, content any, alerts ...Alert) {
	p := Page{
		Title: title, Path: path, Nav: a.nav(), User: a.Me.User.UserName,
		Roles: strings.Join(a.Me.Roles, ", "), Alerts: alerts, Content: content,
	}
	if err := renderPage(w, status, view, p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// fail shows an answer of the service that stopped the view.
func (a *App) fail(w http.ResponseWriter, title, path string, err error) {
	a.show(w, http.StatusBadGateway, "message", title, path, title, Alert{Kind: "error", Text: err.Error()})
}

// part writes HTML a page fetches into itself.
func part(w http.ResponseWriter, name string, data any) {
	if err := renderPart(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// partError is a failure inside a fetched piece: the page around it stays usable.
func partError(w http.ResponseWriter, err error) {
	part(w, "alert", Alert{Kind: "error", Text: err.Error()})
}

// listAll reads every page of a grants query.
func listAll(ctx context.Context, svc Service, q api.ListQuery) ([]api.Grant, error) {
	var all []api.Grant
	for {
		page, err := svc.Grants(ctx, q)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.Cursor == nil {
			return all, nil
		}
		q.Cursor = *page.Cursor
	}
}

func can(g api.Grant, action string) bool { return slices.Contains(g.Actions, action) }

// outcome names a grant's state after an action: "Closed, rejected".
func outcome(g api.Grant) string {
	text := stateLabels[g.State]
	if g.CloseReason != nil {
		text += ", " + strings.ReplaceAll(*g.CloseReason, "_", " ")
	}
	if g.Error != nil {
		text += " (" + *g.Error + ")"
	}
	return text
}
