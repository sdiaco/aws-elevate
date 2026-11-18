package ui

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// newRequestView is the New request view, laid out like the console's
// launch pages: the rules to choose from and the details on the left, a summary with the
// submit button on the right.
type newRequestView struct {
	Rules    []ruleChoice
	Rule     *api.Rule
	Accounts cards
	Form     requestForm
}

// ruleChoice is a rule in the list. Open is set when the caller already has an open
// grant on every account of the rule: nothing is left to ask, so it goes last, greyed.
type ruleChoice struct {
	api.Rule
	Open *api.OpenGrant
}

// cards are accounts shown as Cloudscape cards, to tick (rule form) or pick one
// (new request). An account with an open grant (service's openGrants) is not offered.
type cards struct {
	Multiple bool
	Items    []card
}

type card struct {
	ID      string
	Name    string
	Checked bool
	Open    *api.OpenGrant
}

type requestForm struct {
	Account  string
	Duration string
	Reason   string
}

func (a *App) newRequestPage(w http.ResponseWriter, r *http.Request) {
	a.showNewRequest(w, r, r.URL.Query().Get("rule"), requestForm{})
}

func (a *App) showNewRequest(w http.ResponseWriter, r *http.Request, ruleID string, form requestForm, alerts ...Alert) {
	v, err := a.newRequestView(r, ruleID, form)
	if err != nil {
		a.fail(w, "New request", "/requests", err)
		return
	}
	a.show(w, http.StatusOK, "newrequest", "New request", "/requests", v, alerts...)
}

// newRequestForm is the details form of one rule, fetched when the rule changes.
func (a *App) newRequestForm(w http.ResponseWriter, r *http.Request) {
	v, err := a.newRequestView(r, r.URL.Query().Get("rule"), requestForm{})
	if err != nil {
		partError(w, err)
		return
	}
	part(w, "requestform", v)
}

func (a *App) newRequestView(r *http.Request, ruleID string, form requestForm) (newRequestView, error) {
	rules, err := a.Service.RequestableRules(r.Context())
	if err != nil {
		return newRequestView{}, err
	}
	v := newRequestView{Form: form}
	var covered []ruleChoice
	for _, rule := range rules.Rules {
		c := ruleChoice{Rule: rule, Open: coveredBy(rule, rules.OpenGrants)}
		if c.Open != nil {
			covered = append(covered, c)
		} else {
			v.Rules = append(v.Rules, c)
		}
	}
	v.Rules = append(v.Rules, covered...)
	// A single rule left to ask for leaves nothing to choose; with more, the user picks.
	if ruleID == "" && len(rules.Rules) == 1 && len(covered) == 0 {
		ruleID = rules.Rules[0].ID
	}
	for i := range rules.Rules {
		if rules.Rules[i].ID == ruleID {
			v.Rule = &rules.Rules[i]
		}
	}
	if v.Rule != nil {
		for _, acc := range v.Rule.Accounts {
			v.Accounts.Items = append(v.Accounts.Items, card{
				ID: acc.ID, Name: acc.Name, Open: openOn(v.Rule.ID, acc.ID, rules.OpenGrants),
				Checked: acc.ID == form.Account || len(v.Rule.Accounts) == 1,
			})
		}
		for i := range v.Accounts.Items {
			if v.Accounts.Items[i].Open != nil {
				v.Accounts.Items[i].Checked = false
			}
		}
	}
	return v, nil
}

// openOn is the caller's open grant for the rule on the account, if any.
func openOn(ruleID, account string, open []api.OpenGrant) *api.OpenGrant {
	for i := range open {
		if open[i].RuleID == ruleID && open[i].Account == account {
			return &open[i]
		}
	}
	return nil
}

// coveredBy is an open grant of the rule when every account of it has one, else nil.
func coveredBy(rule api.Rule, open []api.OpenGrant) *api.OpenGrant {
	var first *api.OpenGrant
	for _, acc := range rule.Accounts {
		g := openOn(rule.ID, acc.ID, open)
		if g == nil {
			return nil
		}
		if first == nil {
			first = g
		}
	}
	return first
}

func (a *App) newRequestSubmit(w http.ResponseWriter, r *http.Request) {
	ruleID := r.PostFormValue("rule")
	form := requestForm{Account: r.PostFormValue("account"), Duration: r.PostFormValue("duration"), Reason: r.PostFormValue("reason")}
	seconds, err := parseDuration(form.Duration)
	if err != nil {
		a.showNewRequest(w, r, ruleID, form, Alert{Kind: "error", Text: "Duration: " + err.Error()})
		return
	}
	g, err := a.Service.CreateGrant(r.Context(), api.CreateRequest{
		RuleID: ruleID, Account: form.Account, DurationSeconds: seconds, Reason: form.Reason,
	})
	if err != nil {
		a.showNewRequest(w, r, ruleID, form, errorAlert(err))
		return
	}
	http.Redirect(w, r, "/requests?sent="+g.ID, http.StatusSeeOther)
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

// flash follows the request just sent at the top of Requests, until it is ready to use
// or closed. Kind is info, success or error.
type flash struct {
	Grant   api.Grant
	Kind    string
	Text    string
	Until   time.Time
	Within  bool   // Until is a deadline for someone else: "within", not "left"
	Poll    string // the address that refreshes it, while it changes by itself
	Refresh string // the address a refresh button reads it again from, while pending
	Ready   bool   // AWS accepts the role: the profile is written
	Profile string
	Console string
	Error   string
}

func (a *App) flashOf(g api.Grant) flash {
	f := flash{Grant: g, Kind: "info", Poll: "/requests/flash/" + g.ID}
	switch g.State {
	case api.StatePending:
		// Sent: a person decides later, and the row in the table follows it from here.
		f.Kind, f.Poll, f.Within = "success", "", true
		f.Text = "submitted. An approver in " + approvers(g) + " must approve it"
		if g.ApprovalExpiresAt != nil {
			f.Until = *g.ApprovalExpiresAt
		}
	case api.StateActivating:
		f.Text = "Activating"
	case api.StateActive:
		f.Text = "Active. Waiting for AWS to accept the role"
	default:
		f.Kind, f.Text, f.Poll, f.Error = "error", outcome(g), "", errorOf(g)
	}
	return f
}

// flashPart is the fresh flash; once the grant is active it waits for AWS to accept the
// role and writes the profile before offering the console.
func (a *App) flashPart(w http.ResponseWriter, r *http.Request) {
	d, err := a.Service.Grant(r.Context(), r.PathValue("id"))
	if err != nil {
		partError(w, err)
		return
	}
	f := a.flashOf(d.Grant)
	if d.Grant.State == api.StateActive {
		profile, err := a.Ready(r.Context(), d.Grant)
		if err != nil {
			f.Kind, f.Poll, f.Error = "error", "", err.Error()
		} else {
			f.Kind, f.Text, f.Poll, f.Ready = "success", "Active", "", true
			f.Profile, f.Console = profile, a.Console(d.Grant)
			if d.Grant.ExpiresAt != nil {
				f.Until = *d.Grant.ExpiresAt
			}
		}
	}
	part(w, "flash", f)
}

func approvers(g api.Grant) string {
	if g.ApproverGroup == nil {
		return "an approver"
	}
	return g.ApproverGroup.Name
}
