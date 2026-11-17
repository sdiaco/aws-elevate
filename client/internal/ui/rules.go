package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

// Admin is what the Rules view reads and writes on AWS with the admin's own credentials
// (design §7.2, §7.5).
type Admin interface {
	PermissionSets(ctx context.Context, refresh bool) ([]awsx.Named, error)
	Accounts(ctx context.Context) ([]awsx.Named, error)
	Groups(ctx context.Context) ([]awsx.Named, error)
	// PublishRules creates a rules version on AppConfig starting from latest; it fails
	// with awsx.ErrVersionConflict when another version came out meanwhile.
	PublishRules(ctx context.Context, latest *int, doc []byte) (int, error)
	// PreviewUpdate creates a change set of the stack for infrastructure release v.
	PreviewUpdate(ctx context.Context, v string) (*UpdatePreview, error)
}

// emptyDoc is the document before the first rule (contract/rules/valid-empty.json).
const emptyDoc = `{"schemaVersion":1,"rules":[]}`

// maxImport bounds an imported file; a rules document is a few kilobytes.
const maxImport = 1 << 20

// base is the starting point of a change: the newest valid document and the newest
// version number, valid or not, which AppConfig wants as the version a publication
// starts from.
type base struct {
	Doc     json.RawMessage
	From    *int // version of Doc; nil before the first one
	Latest  *int
	Current *int // version the service uses; behind From for about 30 s after a publish
}

func (a *App) base(ctx context.Context) (base, error) {
	v, err := a.Service.ConfigVersions(ctx)
	if err != nil {
		return base{}, err
	}
	// Not v.Current: for about 30 seconds after a publish the service still uses the
	// previous version, and a change built on it would drop the newer one's changes.
	b := base{Doc: json.RawMessage(emptyDoc), Current: v.Current}
	for _, info := range v.Versions {
		n := info.Number
		if b.Latest == nil || n > *b.Latest {
			b.Latest = &n
		}
		if info.Valid && (b.From == nil || n > *b.From) {
			b.From = &n
		}
	}
	if b.From != nil {
		doc, err := a.Service.ConfigVersion(ctx, *b.From)
		if err != nil {
			return base{}, err
		}
		b.Doc = doc.Doc
	}
	return b, nil
}

// rulesOf reads the rules of a document for display; a document the client cannot read
// is the service's to judge, so it shows as an error.
func rulesOf(doc json.RawMessage) ([]api.Rule, error) {
	var d struct {
		Rules []api.Rule `json:"rules"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("the rules in use cannot be read: %w", err)
	}
	return d.Rules, nil
}

func (a *App) rulesPage(w http.ResponseWriter, r *http.Request) {
	b, rules, sets, err := a.rulesAndSets(r.Context())
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	inUse, err := a.rulesBehind(r.Context(), b)
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	l := List{
		Title: "Rules", Search: true, Empty: "No rule yet.",
		Columns: []string{"Rule", "Status", "Who can request", "Permission set", "Accounts", "Duration", "Approval"},
		Tools:   []Action{{Label: "Import", Path: "/rules/import"}, {Label: "New rule", Path: "/rules/edit", Primary: true}},
	}
	for _, rule := range rules {
		state := ruleState(rule, inUse, sets)
		l.Rows = append(l.Rows, ruleRow(rule, state, Cell{
			Text: rule.ID, Href: "/rules?open=" + url.QueryEscape(rule.ID), Panel: "/rules/panel?id=" + url.QueryEscape(rule.ID),
		}))
		l.Reload = l.Reload || state == "publishing"
	}
	// A rule removed by the newest version shows until the service stops using it.
	for _, rule := range inUse {
		if !slices.ContainsFunc(rules, func(r api.Rule) bool { return r.ID == rule.ID }) {
			row := ruleRow(rule, "deleting", Cell{Text: rule.ID})
			row.ID = "" // in no version to edit: it cannot be selected
			l.Rows = append(l.Rows, row)
			l.Reload = true
		}
	}
	if len(l.Rows) > 0 {
		l.Actions = []Action{{Label: "Delete…", Path: "/rules/delete", Danger: true, Post: true}}
	}
	view := listView{List: l, Notice: a.noticeOf(r)}
	if open := r.URL.Query().Get("open"); open != "" {
		if p := rulePanelOf(rules, sets, open); p != nil {
			view.Panel.Rule = p
			markOpen(view.List.Rows, open)
		} else {
			view.Alert = &Alert{Kind: "error", Text: "No rule " + open + " in the version in use."}
		}
	}
	a.show(w, http.StatusOK, "listpanel", "Rules", "/rules", view)
}

// rulesAndSets reads the newest valid rules and the permission sets of Identity Center.
func (a *App) rulesAndSets(ctx context.Context) (base, []api.Rule, []awsx.Named, error) {
	b, err := a.base(ctx)
	if err != nil {
		return b, nil, nil, err
	}
	rules, err := rulesOf(b.Doc)
	if err != nil {
		return b, nil, nil, err
	}
	sets, err := a.Admin.PermissionSets(ctx, false)
	return b, rules, sets, err
}

// rulesBehind are the rules of the version the service uses, when it is behind the newest
// valid one: only for the 30 seconds or so after a publish, so the list costs one more
// call just then. Otherwise it returns nil, and every rule is in use.
func (a *App) rulesBehind(ctx context.Context, b base) ([]api.Rule, error) {
	if b.Current == nil || b.From == nil || *b.Current == *b.From {
		return nil, nil
	}
	doc, err := a.Service.ConfigVersion(ctx, *b.Current)
	if err != nil {
		return nil, err
	}
	return rulesOf(doc.Doc)
}

// ruleState is the Status of a rule of the newest version: "publishing" while the
// service still uses a version where it is absent or different (design §11.2).
func ruleState(rule api.Rule, inUse []api.Rule, sets []awsx.Named) string {
	if inUse != nil {
		i := slices.IndexFunc(inUse, func(r api.Rule) bool { return r.ID == rule.ID })
		if i < 0 || !reflect.DeepEqual(inUse[i], rule) {
			return "publishing"
		}
	}
	if psMissing(rule, sets) {
		return "ps_missing"
	}
	return api.StateActive
}

func ruleRow(rule api.Rule, state string, id Cell) Row {
	approval := Cell{Text: approvalText(rule)}
	if rule.Privileged {
		approval.Tags = []string{"privileged"}
	}
	return Row{ID: rule.ID, Key: rule.ID, State: state, Cells: []Cell{
		id,
		{State: state},
		{Text: rule.Group.Name},
		{Text: rule.PermissionSet.Name},
		{Text: accountNames(rule)},
		{Text: fmt.Sprintf("%s (max %s)", short(rule.Duration.DefaultSeconds), short(rule.Duration.MaxSeconds))},
		approval,
	}}
}

func psMissing(rule api.Rule, sets []awsx.Named) bool {
	return !slices.ContainsFunc(sets, func(ps awsx.Named) bool { return ps.ID == rule.PermissionSet.Arn })
}

func approvalText(rule api.Rule) string {
	if rule.ApproverGroup != nil {
		return rule.Approval + " by " + rule.ApproverGroup.Name
	}
	return rule.Approval
}

func accountNames(rule api.Rule) string {
	accounts := make([]string, len(rule.Accounts))
	for i, acc := range rule.Accounts {
		accounts[i] = acc.Name + " (" + acc.ID + ")"
	}
	return strings.Join(accounts, ", ")
}

// rulePanel is one rule at the bottom of Rules.
type rulePanel struct {
	Rule      api.Rule
	Accounts  string
	Approval  string
	PSMissing bool
}

func rulePanelOf(rules []api.Rule, sets []awsx.Named, id string) *rulePanel {
	i := slices.IndexFunc(rules, func(r api.Rule) bool { return r.ID == id })
	if i < 0 {
		return nil
	}
	rule := rules[i]
	return &rulePanel{Rule: rule, Accounts: accountNames(rule), Approval: approvalText(rule), PSMissing: psMissing(rule, sets)}
}

// rulePanelPart is the panel of one rule alone, fetched when a row is chosen.
func (a *App) rulePanelPart(w http.ResponseWriter, r *http.Request) {
	_, rules, sets, err := a.rulesAndSets(r.Context())
	if err != nil {
		partError(w, err)
		return
	}
	p := rulePanelOf(rules, sets, r.URL.Query().Get("id"))
	if p == nil {
		partError(w, errors.New("no such rule in the version in use"))
		return
	}
	part(w, "panel", panelSlot{Rule: p})
}

// ruleForm is the one form of a rule (design §3.1), as typed.
type ruleForm struct {
	Orig       string // the id before this edit; empty for a new rule
	ID         string
	Group      string
	PS         string // permission set name
	Accounts   []string
	Default    string
	Max        string
	Approval   string
	Approver   string
	Privileged bool
}

type ruleView struct {
	Form      ruleForm
	Sets      []awsx.Named
	Accounts  []awsx.Named
	Groups    []awsx.Named
	PSMissing bool
}

func formOf(rule api.Rule) ruleForm {
	f := ruleForm{
		Orig: rule.ID, ID: rule.ID, Group: rule.Group.ID, PS: rule.PermissionSet.Name,
		Default: short(rule.Duration.DefaultSeconds), Max: short(rule.Duration.MaxSeconds),
		Approval: rule.Approval, Privileged: rule.Privileged,
	}
	for _, acc := range rule.Accounts {
		f.Accounts = append(f.Accounts, acc.ID)
	}
	if rule.ApproverGroup != nil {
		f.Approver = rule.ApproverGroup.ID
	}
	return f
}

func (a *App) ruleEdit(w http.ResponseWriter, r *http.Request) {
	form := ruleForm{Default: "1h", Max: "4h", Approval: "manual"}
	if id := r.URL.Query().Get("id"); id != "" {
		b, err := a.base(r.Context())
		if err != nil {
			a.fail(w, "Rules", "/rules", err)
			return
		}
		rules, err := rulesOf(b.Doc)
		if err != nil {
			a.fail(w, "Rules", "/rules", err)
			return
		}
		i := slices.IndexFunc(rules, func(x api.Rule) bool { return x.ID == id })
		if i < 0 {
			a.show(w, http.StatusNotFound, "message", "Rules", "/rules", "Rules", Alert{Kind: "error", Text: "No rule " + id + " in the version in use."})
			return
		}
		form = formOf(rules[i])
	}
	a.showRule(w, r, form, r.URL.Query().Get("refresh") == "1")
}

func (a *App) showRule(w http.ResponseWriter, r *http.Request, form ruleForm, refresh bool, alerts ...Alert) {
	v := ruleView{Form: form}
	if err := a.readChoices(r.Context(), &v, refresh); err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	// When the service refused the form, its alert says why; the list here leaves out the
	// reserved sets, so guessing "missing" from it would contradict the alert.
	v.PSMissing = len(alerts) == 0 && form.PS != "" &&
		!slices.ContainsFunc(v.Sets, func(ps awsx.Named) bool { return ps.Name == form.PS })
	title := "New rule"
	if form.Orig != "" {
		title = "Rule " + form.Orig
	}
	a.show(w, http.StatusOK, "rule", title, "/rules", v, alerts...)
}

// choosable drops the permission sets no rule may use: the service refuses them as a
// source (code "reserved"), so the form does not offer them.
func choosable(sets []awsx.Named) []awsx.Named {
	var out []awsx.Named
	for _, ps := range sets {
		if !strings.HasPrefix(ps.Name, "JIT-") && !strings.HasPrefix(ps.Name, "Elevate") {
			out = append(out, ps)
		}
	}
	return out
}

// readChoices fills the lists the rule form chooses from, read from AWS by the admin.
func (a *App) readChoices(ctx context.Context, v *ruleView, refresh bool) error {
	var err error
	if v.Sets, err = a.Admin.PermissionSets(ctx, refresh); err != nil {
		return err
	}
	v.Sets = choosable(v.Sets)
	if v.Accounts, err = a.Admin.Accounts(ctx); err != nil {
		return err
	}
	v.Groups, err = a.Admin.Groups(ctx)
	return err
}

// psPermissions is "What it contains" of the permission set named in the form, read by the
// service (design §7.5): the name comes from the cached list, the content never does.
func (a *App) psPermissions(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("ps")
	sets, err := a.Admin.PermissionSets(r.Context(), false)
	if err != nil {
		partError(w, err)
		return
	}
	i := slices.IndexFunc(sets, func(ps awsx.Named) bool { return ps.Name == name })
	if i < 0 {
		partError(w, fmt.Errorf("no permission set %s in Identity Center: choose one from the list", name))
		return
	}
	p, err := a.Service.PermissionSetPermissions(r.Context(), sets[i].ID)
	if err != nil {
		partError(w, err)
		return
	}
	part(w, "permissions", p)
}

func readForm(r *http.Request) ruleForm {
	return ruleForm{
		Orig: r.PostFormValue("orig"), ID: strings.TrimSpace(r.PostFormValue("id")),
		Group: r.PostFormValue("group"), PS: strings.TrimSpace(r.PostFormValue("ps")),
		Accounts: r.PostForm["account"], Default: strings.TrimSpace(r.PostFormValue("default")),
		Max: strings.TrimSpace(r.PostFormValue("max")), Approval: r.PostFormValue("approval"),
		Approver: r.PostFormValue("approver"), Privileged: r.PostFormValue("privileged") == "on",
	}
}

func find(items []awsx.Named, match func(awsx.Named) bool) (awsx.Named, bool) {
	i := slices.IndexFunc(items, match)
	if i < 0 {
		return awsx.Named{}, false
	}
	return items[i], true
}

func seconds(text string) (int, bool) {
	d, err := time.ParseDuration(text)
	if err != nil || d%time.Second != 0 {
		return 0, false
	}
	return int(d / time.Second), true
}

// ruleOf turns the form into a rule of the document. It only translates what was typed
// (names to ids, 1h to seconds); whether the rule is allowed is the service's check.
func (a *App) ruleOf(ctx context.Context, f ruleForm) (api.Rule, []string, error) {
	sets, err := a.Admin.PermissionSets(ctx, false)
	if err != nil {
		return api.Rule{}, nil, err
	}
	accounts, err := a.Admin.Accounts(ctx)
	if err != nil {
		return api.Rule{}, nil, err
	}
	groups, err := a.Admin.Groups(ctx)
	if err != nil {
		return api.Rule{}, nil, err
	}
	var problems []string
	rule := api.Rule{ID: f.ID, Approval: f.Approval, Privileged: f.Privileged, Accounts: []api.Ref{}}
	if g, ok := find(groups, func(n awsx.Named) bool { return n.ID == f.Group }); ok {
		rule.Group = api.Ref{ID: g.ID, Name: g.Name}
	} else {
		problems = append(problems, "Choose who can request.")
	}
	if ps, ok := find(sets, func(n awsx.Named) bool { return n.Name == f.PS }); ok {
		rule.PermissionSet = api.PermissionSet{Arn: ps.ID, Name: ps.Name}
	} else {
		problems = append(problems, fmt.Sprintf("No permission set named %s.", f.PS))
	}
	for _, id := range f.Accounts {
		if acc, ok := find(accounts, func(n awsx.Named) bool { return n.ID == id }); ok {
			rule.Accounts = append(rule.Accounts, api.Ref{ID: acc.ID, Name: acc.Name})
		}
	}
	def, okDef := seconds(f.Default)
	max, okMax := seconds(f.Max)
	if !okDef || !okMax {
		problems = append(problems, "Durations look like 45m, 1h or 1h30m.")
	}
	rule.Duration = api.Duration{DefaultSeconds: def, MaxSeconds: max}
	if f.Approver != "" {
		if g, ok := find(groups, func(n awsx.Named) bool { return n.ID == f.Approver }); ok {
			rule.ApproverGroup = &api.Ref{ID: g.ID, Name: g.Name}
		}
	}
	return rule, problems, nil
}

func (a *App) ruleSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	form := readForm(r)
	rule, problems, err := a.ruleOf(r.Context(), form)
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	if len(problems) > 0 {
		a.showRule(w, r, form, false, Alert{Kind: "error", Text: strings.Join(problems, " ")})
		return
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	a.review(w, r, change{Kind: "put", Orig: form.Orig, Rule: raw}, func(alerts ...Alert) {
		a.showRule(w, r, form, false, alerts...)
	})
}

func (a *App) rulesDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	ids := r.PostForm["id"]
	if len(ids) == 0 {
		http.Redirect(w, r, "/rules", http.StatusSeeOther)
		return
	}
	a.review(w, r, change{Kind: "delete", IDs: ids}, func(alerts ...Alert) {
		a.show(w, http.StatusOK, "message", "Rules", "/rules", "Rules", alerts...)
	})
}

func (a *App) importPage(w http.ResponseWriter, r *http.Request) {
	a.show(w, http.StatusOK, "import", "Import rules", "/rules", nil)
}

// importSubmit takes a document exported from this or another installation (design §7.4).
func (a *App) importSubmit(w http.ResponseWriter, r *http.Request) {
	back := func(alerts ...Alert) { a.show(w, http.StatusOK, "import", "Import rules", "/rules", nil, alerts...) }
	file, _, err := r.FormFile("doc")
	if err != nil {
		back(Alert{Kind: "error", Text: "Choose a rules file."})
		return
	}
	defer file.Close()
	doc, err := io.ReadAll(io.LimitReader(file, maxImport+1))
	switch {
	case err != nil:
		back(errorAlert(err))
		return
	case len(doc) > maxImport:
		back(Alert{Kind: "error", Text: "The file is larger than 1 MB: it is not a rules document."})
		return
	case !json.Valid(doc):
		back(Alert{Kind: "error", Text: "The file is not valid JSON."})
		return
	}
	a.review(w, r, change{Kind: "replace", Doc: doc}, back)
}
