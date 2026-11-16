package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
}

// emptyDoc is the document before the first rule (contract/rules/valid-empty.json).
const emptyDoc = `{"schemaVersion":1,"rules":[]}`

// maxImport bounds an imported file; a rules document is a few kilobytes.
const maxImport = 1 << 20

// base is the starting point of a change: the document in use and the newest version
// number, valid or not, which AppConfig wants as the version a publication starts from.
type base struct {
	Doc    json.RawMessage
	From   *int // version of Doc; nil before the first one
	Latest *int
}

func (a *App) base(ctx context.Context) (base, error) {
	v, err := a.Service.ConfigVersions(ctx)
	if err != nil {
		return base{}, err
	}
	b := base{Doc: json.RawMessage(emptyDoc), From: v.Current}
	for _, info := range v.Versions {
		if b.Latest == nil || info.Number > *b.Latest {
			n := info.Number
			b.Latest = &n
		}
	}
	if v.Current != nil {
		doc, err := a.Service.ConfigVersion(ctx, *v.Current)
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
	sets, err := a.Admin.PermissionSets(r.Context(), false)
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	l := List{
		Title: "Rules", Search: true, Empty: "No rule yet.",
		Columns: []string{"Rule", "Who can request", "Permission set", "Accounts", "Duration", "Approval"},
		Tools:   []Action{{Label: "Import", Path: "/rules/import"}, {Label: "New rule", Path: "/rules/edit"}},
	}
	for _, rule := range rules {
		var tags []string
		if !slices.ContainsFunc(sets, func(ps awsx.Named) bool { return ps.ID == rule.PermissionSet.Arn }) {
			tags = append(tags, "PS not found")
		}
		accounts := make([]string, len(rule.Accounts))
		for i, acc := range rule.Accounts {
			accounts[i] = acc.Name + " (" + acc.ID + ")"
		}
		approval := Cell{Text: rule.Approval}
		if rule.ApproverGroup != nil {
			approval.Text += " by " + rule.ApproverGroup.Name
		}
		if rule.Privileged {
			approval.Tags = []string{"privileged"}
		}
		l.Rows = append(l.Rows, Row{ID: rule.ID, Cells: []Cell{
			{Text: rule.ID, Href: "/rules/edit?id=" + rule.ID},
			{Text: rule.Group.Name},
			{Text: rule.PermissionSet.Name, Tags: tags},
			{Text: strings.Join(accounts, ", ")},
			{Text: fmt.Sprintf("%s (max %s)", short(rule.Duration.DefaultSeconds), short(rule.Duration.MaxSeconds))},
			approval,
		}})
	}
	if len(l.Rows) > 0 {
		l.Actions = []Action{{Label: "Delete…", Path: "/rules/delete", Danger: true}}
	}
	a.show(w, http.StatusOK, "list", "Rules", "/rules", l)
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
	// Contents is loaded from the service for a saved rule whose permission set is kept.
	Contents string
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
	var err error
	if v.Sets, err = a.Admin.PermissionSets(r.Context(), refresh); err == nil {
		if v.Accounts, err = a.Admin.Accounts(r.Context()); err == nil {
			v.Groups, err = a.Admin.Groups(r.Context())
		}
	}
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	v.PSMissing = form.PS != "" && !slices.ContainsFunc(v.Sets, func(ps awsx.Named) bool { return ps.Name == form.PS })
	if form.Orig != "" && !v.PSMissing {
		v.Contents = "/request/permissions?rule=" + form.Orig
	}
	title := "New rule"
	if form.Orig != "" {
		title = "Rule " + form.Orig
	}
	a.show(w, http.StatusOK, "rule", title, "/rules", v, alerts...)
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
		back(Alert{Kind: "error", Text: err.Error()})
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
