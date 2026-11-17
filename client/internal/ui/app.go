package ui

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// Service is the part of the api client the views use. Every decision comes from it:
// the views show the actions each grant carries and nothing else.
type Service interface {
	Rules(ctx context.Context) (*api.RuleList, error)
	RequestableRules(ctx context.Context) (*api.RuleList, error)
	CheckRules(ctx context.Context, doc []byte) (*api.CheckResult, error)
	Permissions(ctx context.Context, ruleID string) (*api.Permissions, error)
	PermissionSetPermissions(ctx context.Context, arn string) (*api.Permissions, error)
	CreateGrant(ctx context.Context, req api.CreateRequest) (*api.Grant, error)
	Grant(ctx context.Context, id string) (*api.GrantDetail, error)
	Grants(ctx context.Context, q api.ListQuery) (*api.GrantList, error)
	Approve(ctx context.Context, id string) (*api.Grant, error)
	Reject(ctx context.Context, id string) (*api.Grant, error)
	Cancel(ctx context.Context, id string) (*api.Grant, error)
	Revoke(ctx context.Context, id, reason string) (*api.Grant, error)
	Redrive(ctx context.Context, id string) (*api.Grant, error)
	ConfigVersions(ctx context.Context) (*api.ConfigVersions, error)
	ConfigVersion(ctx context.Context, n int) (*api.ConfigVersion, error)
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
	// Admin reads and writes AWS with the admin's credentials; nil for other roles.
	Admin Admin
	// Updates lists releases and updates the client; nil turns the Updates view off.
	Updates *Updates

	skippedWelcome atomic.Bool // the admin skipped the first run in this session
}

func (a *App) has(role string) bool { return slices.Contains(a.Me.Roles, role) }

// Pages are the views of the interface. A view the roles do not allow is not served.
func Pages(a *App) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticFiles())
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /requests", a.requestsPage)
	if a.has("requester") {
		mux.HandleFunc("GET /requests/new", a.newRequestPage)
		mux.HandleFunc("POST /requests/new", a.newRequestSubmit)
		mux.HandleFunc("GET /requests/new/form", a.newRequestForm)
	}
	mux.HandleFunc("GET /requests/permissions", a.permissions)
	mux.HandleFunc("GET /requests/flash/{id}", a.flashPart)
	mux.HandleFunc("GET /requests/row/{id}", a.requestRow)
	mux.HandleFunc("GET /panel/{id}", a.panelPart)
	mux.HandleFunc("GET /requests/outcome/{id}", a.outcomeRow)
	mux.HandleFunc("POST /requests/approve", a.approve)
	mux.HandleFunc("GET /requests/reject", a.rejectConfirm)
	mux.HandleFunc("POST /requests/reject", a.reject)
	mux.HandleFunc("GET /requests/cancel", a.cancelConfirm)
	mux.HandleFunc("POST /requests/cancel", a.cancel)
	mux.HandleFunc("GET /requests/revoke", a.revokeConfirm)
	mux.HandleFunc("POST /requests/revoke", a.revoke)
	if a.has("admin") {
		mux.HandleFunc("POST /requests/retry", a.retry)
	}
	mux.HandleFunc("GET /history", a.historyPage)
	mux.HandleFunc("GET /history/export.csv", a.historyExport)
	if a.has("admin") && a.Admin != nil {
		mux.HandleFunc("GET /rules", a.rulesPage)
		mux.HandleFunc("GET /rules/edit", a.ruleEdit)
		mux.HandleFunc("GET /rules/panel", a.rulePanelPart)
		mux.HandleFunc("GET /rules/ps-permissions", a.psPermissions)
		mux.HandleFunc("POST /rules/edit", a.ruleSave)
		mux.HandleFunc("POST /rules/delete", a.rulesDelete)
		mux.HandleFunc("GET /rules/import", a.importPage)
		mux.HandleFunc("POST /rules/import", a.importSubmit)
		mux.HandleFunc("POST /rules/publish", a.publish)
		mux.HandleFunc("GET /welcome", a.welcome)
		mux.HandleFunc("POST /welcome/skip", a.skipWelcome)
	}
	if a.has("admin") && a.Admin != nil && a.Updates != nil {
		mux.HandleFunc("GET /updates", a.updatesPage)
		mux.HandleFunc("POST /updates/client", a.updateClient)
		mux.HandleFunc("POST /updates/preview", a.preview)
	}
	if a.has("admin") || a.has("auditor") {
		mux.HandleFunc("GET /versions", a.versionsPage)
		mux.HandleFunc("GET /versions/compare", a.compare)
		mux.HandleFunc("GET /versions/{n}/panel", a.versionPanelPart)
		mux.HandleFunc("GET /versions/{n}/export.json", a.versionExport)
	}
	return mux
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	first, err := a.firstRun(r.Context())
	if err != nil {
		a.fail(w, "aws-elevate", "/", err)
		return
	}
	if first {
		http.Redirect(w, r, "/welcome", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/requests", http.StatusSeeOther)
}

func (a *App) nav() []NavGroup {
	groups := []NavGroup{{Items: []NavItem{
		{Label: "Requests", Path: "/requests"},
		{Label: "History", Path: "/history"},
	}}}
	var admin []NavItem
	if a.has("admin") && a.Admin != nil {
		admin = append(admin, NavItem{Label: "Rules", Path: "/rules"})
	}
	if a.has("admin") || a.has("auditor") {
		admin = append(admin, NavItem{Label: "Config versions", Path: "/versions"})
	}
	if a.has("admin") && a.Admin != nil && a.Updates != nil {
		admin = append(admin, NavItem{Label: "Updates", Path: "/updates"})
	}
	if len(admin) > 0 {
		groups = append(groups, NavGroup{Label: "Administration", Items: admin})
	}
	return groups
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
