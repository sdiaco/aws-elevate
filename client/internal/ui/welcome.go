package ui

import (
	"context"
	"net/http"
)

// firstRun is true for an admin with AWS access while no rule exists and the first run
// was not skipped in this session (design §11.2, Primo avvio).
func (a *App) firstRun(ctx context.Context) (bool, error) {
	if !a.has("admin") || a.Admin == nil || a.skippedWelcome.Load() {
		return false, nil
	}
	list, err := a.Service.Rules(ctx)
	if err != nil {
		return false, err
	}
	return len(list.Rules) == 0, nil
}

// welcome is the first run: the rule form in four steps (what, where, who, how), full
// screen. It saves through the same review and publication as the Rules view.
func (a *App) welcome(w http.ResponseWriter, r *http.Request) {
	list, err := a.Service.Rules(r.Context())
	if err != nil {
		a.fail(w, "Your first rule", "/rules", err)
		return
	}
	if len(list.Rules) > 0 {
		http.Redirect(w, r, "/rules", http.StatusSeeOther)
		return
	}
	v := ruleView{Form: ruleForm{Default: "1h", Max: "4h", Approval: "manual"}}
	if err := a.readChoices(r.Context(), &v, r.URL.Query().Get("refresh") == "1"); err != nil {
		a.fail(w, "Your first rule", "/rules", err)
		return
	}
	p := Page{Title: "Your first rule", Path: "/welcome", User: a.Me.User.UserName, Content: v, Bare: true}
	if err := renderPage(w, http.StatusOK, "welcome", p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) skipWelcome(w http.ResponseWriter, r *http.Request) {
	a.skippedWelcome.Store(true)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
