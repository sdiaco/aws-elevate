package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// firstRun is true for an admin with AWS access while no rule exists and the first run
// was not skipped in this session.
func (a *App) firstRun(ctx context.Context) (bool, error) {
	if !a.has("admin") || a.Admin == nil || a.skippedWelcome.Load() {
		return false, nil
	}
	rules, err := a.rulesInUse(ctx)
	if err != nil {
		return false, err
	}
	return len(rules) == 0, nil
}

// rulesInUse reads the rules from the version in use, like the Rules view: before the
// first publication GET /rules answers config_unavailable, the version list is empty.
func (a *App) rulesInUse(ctx context.Context) ([]api.Rule, error) {
	b, err := a.base(ctx)
	if err != nil {
		return nil, err
	}
	return rulesOf(b.Doc)
}

// welcome is the first run: the rule form in steps (what, where, who, how, review), full
// screen. It publishes through the same check and publication as the Rules view.
func (a *App) welcome(w http.ResponseWriter, r *http.Request) {
	rules, err := a.rulesInUse(r.Context())
	if err != nil {
		a.fail(w, "Your first rule", "/rules", err)
		return
	}
	if len(rules) > 0 {
		http.Redirect(w, r, "/rules", http.StatusSeeOther)
		return
	}
	a.showWelcome(w, r, ruleForm{Default: "1h", Max: "4h", Approval: "manual"}, nil)
}

// firstReview is the last step of the first run: the rule as the service accepted it.
type firstReview struct {
	Rule   api.Rule
	JSON   string
	Change string // what /rules/publish applies
	Latest string // the version the change was made on; empty before the first one
}

// reviewFirstRule checks the rule like any change, then shows it as the wizard's last
// step instead of the Review page: there is nothing to compare it with yet.
func (a *App) reviewFirstRule(w http.ResponseWriter, r *http.Request, form ruleForm, rule api.Rule) {
	raw, err := json.Marshal(rule)
	if err != nil {
		a.fail(w, "Your first rule", "/rules", err)
		return
	}
	b, err := a.base(r.Context())
	if err != nil {
		a.fail(w, "Your first rule", "/rules", err)
		return
	}
	c := change{Kind: "put", Rule: raw}
	if _, refused := a.checked(r.Context(), b, c); refused != nil {
		a.showWelcome(w, r, form, nil, refused...)
		return
	}
	pretty, err := json.MarshalIndent(rule, "", "  ")
	if err != nil {
		a.fail(w, "Your first rule", "/rules", err)
		return
	}
	cj, err := json.Marshal(c)
	if err != nil {
		a.fail(w, "Your first rule", "/rules", err)
		return
	}
	review := &firstReview{Rule: rule, JSON: string(pretty), Change: string(cj)}
	if b.Latest != nil {
		review.Latest = strconv.Itoa(*b.Latest)
	}
	a.showWelcome(w, r, form, review)
}

func (a *App) showWelcome(w http.ResponseWriter, r *http.Request, form ruleForm, review *firstReview, alerts ...Alert) {
	v := ruleView{Form: form, Review: review}
	if err := a.readChoices(r.Context(), &v, r.URL.Query().Get("refresh") == "1"); err != nil {
		a.fail(w, "Your first rule", "/rules", err)
		return
	}
	p := Page{Title: "Your first rule", Path: "/welcome", User: a.me().User.UserName, Organization: a.me().OrganizationName, Content: v, Alerts: alerts, Bare: true, Closed: a.closedAlert()}
	if err := renderPage(w, http.StatusOK, "welcome", p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) skipWelcome(w http.ResponseWriter, r *http.Request) {
	a.skippedWelcome.Store(true)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
