package ui

import (
	"context"
	"net/http"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// pendingView is the Pending view (design §11.2): one card per request.
type pendingView struct {
	ToApprove []api.Grant
	Mine      []api.Grant
}

func (a *App) pendingPage(w http.ResponseWriter, r *http.Request) {
	a.showPending(w, r)
}

func (a *App) showPending(w http.ResponseWriter, r *http.Request, alerts ...Alert) {
	v, err := a.pending(r.Context())
	if err != nil {
		a.fail(w, "Pending", "/pending", err)
		return
	}
	a.show(w, http.StatusOK, "pending", "Pending", "/pending", v, alerts...)
}

func (a *App) pending(ctx context.Context) (pendingView, error) {
	var v pendingView
	approvable, err := listAll(ctx, a.Service, api.ListQuery{Scope: "approvable"})
	if err != nil {
		return v, err
	}
	for _, g := range approvable {
		if g.State == api.StatePending && (can(g, "approve") || can(g, "reject")) {
			v.ToApprove = append(v.ToApprove, g)
		}
	}
	mine, err := listAll(ctx, a.Service, api.ListQuery{Scope: "mine", Open: true})
	if err != nil {
		return v, err
	}
	for _, g := range mine {
		if g.State == api.StatePending {
			v.Mine = append(v.Mine, g)
		}
	}
	return v, nil
}

// decide runs an action on one request and shows its outcome above the view.
func (a *App) decide(w http.ResponseWriter, r *http.Request, act func(ctx context.Context, id string) (*api.Grant, error)) {
	id := r.PostFormValue("id")
	g, err := act(r.Context(), id)
	if err != nil {
		a.showPending(w, r, Alert{Kind: "error", Text: id + ": " + err.Error()})
		return
	}
	a.showPending(w, r, Alert{Kind: "success", Text: "Request " + id + ": " + outcome(*g)})
}

func (a *App) approve(w http.ResponseWriter, r *http.Request) { a.decide(w, r, a.Service.Approve) }
func (a *App) reject(w http.ResponseWriter, r *http.Request)  { a.decide(w, r, a.Service.Reject) }
func (a *App) cancel(w http.ResponseWriter, r *http.Request)  { a.decide(w, r, a.Service.Cancel) }

func (a *App) rejectConfirm(w http.ResponseWriter, r *http.Request) {
	a.confirmRequest(w, r, "Reject request", "Reject", "/pending/reject")
}

func (a *App) cancelConfirm(w http.ResponseWriter, r *http.Request) {
	a.confirmRequest(w, r, "Cancel request", "Cancel request", "/pending/cancel")
}

func (a *App) confirmRequest(w http.ResponseWriter, r *http.Request, title, button, action string) {
	id := r.URL.Query().Get("id")
	d, err := a.Service.Grant(r.Context(), id)
	if err != nil {
		a.fail(w, title, "/pending", err)
		return
	}
	g := d.Grant
	a.show(w, http.StatusOK, "confirm", title, "/pending", confirm{
		Title: title, Action: action, IDs: []string{id}, Button: button, Back: "/pending",
		Text: g.Requester.UserName + " asks " + g.SourcePs.Name + " on " + g.Account + ": " + g.Reason,
	})
}
