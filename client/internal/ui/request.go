package ui

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// requestView is the Request view (design §11.2): the rules on the left, the form of the
// chosen one on the right.
type requestView struct {
	Rules    []api.Rule
	Rule     *api.Rule
	Accounts []option
	Form     requestForm
}

type option struct {
	Value    string
	Label    string
	Selected bool
}

type requestForm struct {
	Account  string
	Duration string
	Reason   string
}

func (a *App) requestPage(w http.ResponseWriter, r *http.Request) {
	a.showRequest(w, r, r.URL.Query().Get("rule"), requestForm{})
}

func (a *App) showRequest(w http.ResponseWriter, r *http.Request, ruleID string, form requestForm, alerts ...Alert) {
	rules, err := a.Service.RequestableRules(r.Context())
	if err != nil {
		a.fail(w, "Request access", "/request", err)
		return
	}
	v := requestView{Rules: rules.Rules, Form: form}
	for i := range rules.Rules {
		if rules.Rules[i].ID == ruleID {
			v.Rule = &rules.Rules[i]
		}
	}
	if v.Rule != nil {
		for _, acc := range v.Rule.Accounts {
			v.Accounts = append(v.Accounts, option{
				Value: acc.ID, Label: acc.Name + " (" + acc.ID + ")",
				Selected: acc.ID == form.Account || len(v.Rule.Accounts) == 1,
			})
		}
	}
	a.show(w, http.StatusOK, "request", "Request access", "/request", v, alerts...)
}

func (a *App) requestSubmit(w http.ResponseWriter, r *http.Request) {
	ruleID := r.PostFormValue("rule")
	form := requestForm{Account: r.PostFormValue("account"), Duration: r.PostFormValue("duration"), Reason: r.PostFormValue("reason")}
	seconds, err := parseDuration(form.Duration)
	if err != nil {
		a.showRequest(w, r, ruleID, form, Alert{Kind: "error", Text: "Duration: " + err.Error()})
		return
	}
	g, err := a.Service.CreateGrant(r.Context(), api.CreateRequest{
		RuleID: ruleID, Account: form.Account, DurationSeconds: seconds, Reason: form.Reason,
	})
	if err != nil {
		a.showRequest(w, r, ruleID, form, Alert{Kind: "error", Text: err.Error()})
		return
	}
	http.Redirect(w, r, "/request/"+g.ID, http.StatusSeeOther)
}

// parseDuration reads 45m, 2h or 1h30m; empty leaves the rule default to the service.
func parseDuration(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d%time.Second != 0 {
		return 0, fmt.Errorf("%q is not a duration like 45m or 2h", s)
	}
	return int(d / time.Second), nil
}

// permissions is "What you will get" of a rule, read when the reader opens it.
func (a *App) permissions(w http.ResponseWriter, r *http.Request) {
	p, err := a.Service.Permissions(r.Context(), r.URL.Query().Get("rule"))
	if err != nil {
		partError(w, err)
		return
	}
	part(w, "permissions", p)
}

// progress is a request after it is sent: its steps, then the ready command.
type progress struct {
	Grant   api.Grant
	Steps   []step
	Poll    string // the address that refreshes it, while it can still change
	Ready   bool   // AWS accepts the role: the profile is written
	Profile string
	Console string
	Alert   *Alert
}

type step struct {
	Label  string
	Status string // done, current, failed
	Until  time.Time
}

func (a *App) progressPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, err := a.Service.Grant(r.Context(), id)
	if err != nil {
		a.fail(w, "Request "+id, "/request", err)
		return
	}
	// The page shows at once; the first refresh waits for the role.
	a.show(w, http.StatusOK, "progress", "Request "+id, "/request", a.progress(d.Grant))
}

func (a *App) progressPart(w http.ResponseWriter, r *http.Request) {
	d, err := a.Service.Grant(r.Context(), r.PathValue("id"))
	if err != nil {
		partError(w, err)
		return
	}
	p := a.progress(d.Grant)
	if d.Grant.State == api.StateActive {
		profile, err := a.Ready(r.Context(), d.Grant)
		if err != nil {
			p.Poll = ""
			p.Alert = &Alert{Kind: "error", Text: err.Error()}
		} else {
			p = a.ready(d.Grant, profile)
		}
	}
	part(w, "progress", p)
}

func (a *App) progress(g api.Grant) progress {
	p := progress{Grant: g, Steps: []step{{Label: "Sent", Status: "done"}}}
	add := func(label, status string) { p.Steps = append(p.Steps, step{Label: label, Status: status}) }
	if g.Approval == "manual" {
		switch {
		case g.State == api.StatePending:
			s := step{Label: "Waiting for approval by " + approvers(g), Status: "current"}
			if g.ApprovalExpiresAt != nil {
				s.Until = *g.ApprovalExpiresAt
			}
			p.Steps = append(p.Steps, s)
		case g.Approver != nil:
			add("Approved by "+g.Approver.UserName, "done")
		case g.ActivatedAt != nil:
			add("Approved", "done")
		}
	}
	switch g.State {
	case api.StatePending:
		p.Poll = "/request/" + g.ID + "/progress"
	case api.StateActivating:
		add("Activating", "current")
		p.Poll = "/request/" + g.ID + "/progress"
	case api.StateActive:
		add("Activated", "done")
		add("Waiting for AWS to accept the role", "current")
		p.Poll = "/request/" + g.ID + "/progress"
	default:
		add(outcome(g), "failed")
	}
	return p
}

func (a *App) ready(g api.Grant, profile string) progress {
	p := a.progress(g)
	p.Steps = p.Steps[:len(p.Steps)-1]
	until := step{Label: "Active", Status: "done"}
	if g.ExpiresAt != nil {
		until.Until = *g.ExpiresAt
	}
	p.Steps = append(p.Steps, until)
	p.Poll, p.Ready, p.Profile, p.Console = "", true, profile, a.Console(g)
	return p
}

func approvers(g api.Grant) string {
	if g.ApproverGroup == nil {
		return "an approver"
	}
	return g.ApproverGroup.Name
}
