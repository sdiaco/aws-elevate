package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

// change is what the admin asked, kept apart from the document it applies to so that,
// when another version comes out meanwhile, it can be applied again to that version:
// put a rule (replacing Orig), delete rules, or replace the document.
type change struct {
	Kind string          `json:"kind"`
	Orig string          `json:"orig,omitempty"`
	Rule json.RawMessage `json:"rule,omitempty"`
	IDs  []string        `json:"ids,omitempty"`
	Doc  json.RawMessage `json:"doc,omitempty"`
}

// apply returns the document with the change; other rules and fields stay as they are.
func (c change) apply(doc json.RawMessage) (json.RawMessage, error) {
	if c.Kind == "replace" {
		return c.Doc, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return nil, fmt.Errorf("the rules in use cannot be read: %w", err)
	}
	var rules []json.RawMessage
	if err := json.Unmarshal(top["rules"], &rules); err != nil {
		return nil, fmt.Errorf("the rules in use cannot be read: %w", err)
	}
	idOf := func(raw json.RawMessage) string {
		var head struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &head) // a rule without an id matches nothing
		return head.ID
	}
	switch c.Kind {
	case "put":
		i := slices.IndexFunc(rules, func(raw json.RawMessage) bool { return c.Orig != "" && idOf(raw) == c.Orig })
		if i < 0 {
			rules = append(rules, c.Rule)
		} else {
			kept, err := keepNames(rules[i], c.Rule)
			if err != nil {
				return nil, err
			}
			rules[i] = kept
		}
	case "delete":
		rules = slices.DeleteFunc(rules, func(raw json.RawMessage) bool { return slices.Contains(c.IDs, idOf(raw)) })
	default:
		return nil, fmt.Errorf("unknown change %q", c.Kind)
	}
	if rules == nil {
		rules = []json.RawMessage{}
	}
	out, err := json.Marshal(rules)
	if err != nil {
		return nil, err
	}
	top["rules"] = out
	return json.Marshal(top)
}

// keepNames gives the edited rule the names in use of the accounts and groups it keeps:
// Organizations or Identity Center may have renamed them meanwhile, and the review shows
// only what the admin changed.
func keepNames(old, edited json.RawMessage) (json.RawMessage, error) {
	var was, now api.Rule
	if err := json.Unmarshal(old, &was); err != nil {
		return edited, nil // a rule in use that cannot be read keeps nothing
	}
	if err := json.Unmarshal(edited, &now); err != nil {
		return nil, fmt.Errorf("the edited rule cannot be read: %w", err)
	}
	if now.Group.ID == was.Group.ID {
		now.Group.Name = was.Group.Name
	}
	if now.ApproverGroup != nil && was.ApproverGroup != nil && now.ApproverGroup.ID == was.ApproverGroup.ID {
		now.ApproverGroup.Name = was.ApproverGroup.Name
	}
	for i, acc := range now.Accounts {
		if j := slices.IndexFunc(was.Accounts, func(a api.Ref) bool { return a.ID == acc.ID }); j >= 0 {
			now.Accounts[i].Name = was.Accounts[j].Name
		}
	}
	return json.Marshal(now)
}

// touches are the rule ids the change rewrites; nil means every rule.
func (c change) touches() []string {
	switch c.Kind {
	case "put":
		var head struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(c.Rule, &head)
		return []string{c.Orig, head.ID}
	case "delete":
		return c.IDs
	}
	return nil
}

type reviewView struct {
	Changes compareView
	Change  string
	Latest  string
	// Replaces are rules another admin changed meanwhile that this publish overwrites:
	// publishing them needs one more confirmation.
	Replaces []string
	// Deletes are the rules this publish removes; deleting asks once more in a dialog
	//.
	Deletes     []string
	DeleteTitle string
}

// review asks the service to check the changed document, then shows what changes and
// waits for confirmation (no drafts: summary and confirmation). On a check
// error, back shows the page the admin came from with the errors.
func (a *App) review(w http.ResponseWriter, r *http.Request, c change, back func(...Alert), alerts ...Alert) {
	b, err := a.base(r.Context())
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	a.reviewOn(w, r, b, c, back, nil, alerts...)
}

// checked applies c on the document of b and has the service check the result. A refusal
// comes back as the alerts that say why.
func (a *App) checked(ctx context.Context, b base, c change) ([]byte, []Alert) {
	doc, err := c.apply(b.Doc)
	if err != nil {
		return nil, []Alert{errorAlert(err)}
	}
	check, err := a.Service.CheckRules(ctx, doc)
	if err != nil {
		return nil, []Alert{errorAlert(err)}
	}
	if !check.Valid {
		return nil, checkAlerts(check.Errors)
	}
	return doc, nil
}

func (a *App) reviewOn(w http.ResponseWriter, r *http.Request, b base, c change, back func(...Alert), replaces []string, alerts ...Alert) {
	doc, refused := a.checked(r.Context(), b, c)
	if refused != nil {
		back(refused...)
		return
	}
	diff, err := diffRules(b.Doc, doc)
	if err != nil {
		back(errorAlert(err))
		return
	}
	raw, err := json.Marshal(c)
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	diff.OldLabel, diff.NewLabel = "In use", "After publishing"
	v := reviewView{Changes: diff, Change: string(raw), Replaces: replaces}
	for _, rc := range diff.Removed {
		v.Deletes = append(v.Deletes, rc.ID)
	}
	v.DeleteTitle = "Delete " + count(len(v.Deletes), "rule", "rules") + "?"
	if b.Latest != nil {
		v.Latest = strconv.Itoa(*b.Latest)
	}
	a.show(w, http.StatusOK, "review", "Review changes", "/rules", v, alerts...)
}

// formFields maps a rule field, as the service names it in a check error, to the form
// field that edits it and its label.
var formFields = map[string]struct{ ID, Label string }{
	"id":                      {"id", "Name"},
	"permissionSet":           {"ps", "Permission set"},
	"accounts":                {"accounts", "Accounts"},
	"group":                   {"group", "Who can request"},
	"approval":                {"approval", "Approval"},
	"approverGroup":           {"approver", "Who approves"},
	"duration":                {"default", "Duration"},
	"duration.defaultSeconds": {"default", "Proposed duration"},
	"duration.maxSeconds":     {"max", "Maximum duration"},
	"privileged":              {"privileged", "Privileged"},
}

// formField finds the form field of a service field; a nested one such as
// accounts[1].id or permissionSet.arn belongs to the field it starts with.
func formField(field string) (id, label string, ok bool) {
	f, ok := formFields[field]
	if !ok {
		f, ok = formFields[strings.FieldsFunc(field, func(r rune) bool { return r == '.' || r == '[' })[0]]
	}
	return f.ID, f.Label, ok
}

func checkAlerts(errs []api.CheckError) []Alert {
	out := make([]Alert, len(errs))
	for i, e := range errs {
		where := "Document"
		if e.RuleID != nil {
			where = "Rule " + *e.RuleID
		}
		alert := Alert{Kind: "error", Text: where + ": " + e.Message}
		if e.Field != nil && *e.Field != "" {
			if id, label, ok := formField(*e.Field); ok {
				alert.Text = where + " · " + label + ": " + e.Message
				alert.Field, alert.FieldText = id, e.Message
			}
		}
		out[i] = alert
	}
	return out
}

// fieldErrors are the messages to show under the form fields: the service's, with the
// first one winning when a field has several.
func fieldErrors(alerts []Alert) map[string]string {
	out := map[string]string{}
	for _, a := range alerts {
		if _, seen := out[a.Field]; a.Field != "" && !seen {
			out[a.Field] = a.FieldText
		}
	}
	return out
}

// publish writes the reviewed change on AppConfig, starting from the version the review
// was made on. If another version came out meanwhile, the change is reviewed again on it.
func (a *App) publish(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	var c change
	if err := json.Unmarshal([]byte(r.PostFormValue("change")), &c); err != nil {
		a.fail(w, "Rules", "/rules", fmt.Errorf("the change to publish cannot be read: %w", err))
		return
	}
	var started *int
	if n, err := strconv.Atoi(r.PostFormValue("latest")); err == nil {
		started = &n
	}
	back := func(alerts ...Alert) { a.show(w, http.StatusOK, "message", "Rules", "/rules", "Rules", alerts...) }
	b, err := a.base(r.Context())
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	if !samePtr(b.Latest, started) {
		a.conflict(w, r, b, c, started, back)
		return
	}
	if replaces := r.PostFormValue("replaces"); replaces != "" && r.PostFormValue("confirmed") != "1" {
		a.reviewOn(w, r, b, c, back, strings.Split(replaces, ","), Alert{Kind: "warning",
			Text: "Publishing replaces another admin's change to " + strings.ReplaceAll(replaces, ",", ", ") + ": confirm it to go on."})
		return
	}
	doc, err := c.apply(b.Doc)
	if err != nil {
		back(errorAlert(err))
		return
	}
	n, err := a.Admin.PublishRules(r.Context(), b.Latest, doc)
	if errors.Is(err, awsx.ErrVersionConflict) {
		if b, err = a.base(r.Context()); err != nil {
			a.fail(w, "Rules", "/rules", err)
			return
		}
		a.conflict(w, r, b, c, started, back)
		return
	}
	if err != nil {
		a.fail(w, "Rules", "/rules", fmt.Errorf("publish the rules on AppConfig: %w", err))
		return
	}
	text := c.published(n)
	if b.Latest != nil && n <= *b.Latest {
		text = fmt.Sprintf("No change: version %d already has this content.", n)
	}
	a.redirectWithNotice(w, r, "/rules", notice{Kind: "success", Text: text})
}

// published says what is happening to the rules after version n went out: the service
// reads a new version within about 30 seconds, so the list still shows the old one.
func (c change) published(n int) string {
	later := fmt.Sprintf(" The list shows it once the service has read version %d.", n)
	switch c.Kind {
	case "delete":
		if len(c.IDs) == 1 {
			return "Deleting rule " + c.IDs[0] + ". New requests can no longer use it within about 30 seconds." + later
		}
		return fmt.Sprintf("Deleting %d rules: %s. New requests can no longer use them within about 30 seconds.",
			len(c.IDs), strings.Join(c.IDs, ", ")) + later
	case "put":
		var head struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(c.Rule, &head)
		if c.Orig == "" {
			return "Saving rule " + head.ID + ". New requests can use it within about 30 seconds." + later
		}
		return "Saving the changes to rule " + head.ID + ". They apply to new requests within about 30 seconds." + later
	}
	return "Publishing the imported rules. They apply to new requests within about 30 seconds." + later
}

func samePtr(x, y *int) bool { return (x == nil && y == nil) || (x != nil && y != nil && *x == *y) }

// conflict says who published after the admin started and reviews the change again on
// the new version. When that version changed the same rules, publishing replaces their
// change and needs a confirmation.
func (a *App) conflict(w http.ResponseWriter, r *http.Request, b base, c change, started *int, back func(...Alert)) {
	versions, err := a.Service.ConfigVersions(r.Context())
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	var text []string
	for _, v := range versions.Versions {
		if started == nil || v.Number > *started {
			// CloudTrail records the author a few minutes after the publish.
			who := "Another admin"
			if v.Author != nil {
				who = *v.Author
			}
			text = append(text, fmt.Sprintf("%s published version %d while you were editing.", who, v.Number))
		}
	}
	same, err := a.changedMeanwhile(r.Context(), b, c, started, versions)
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	now := "The review below now starts from version " + latestText(b.Latest)
	if len(same) > 0 {
		text = append(text, "It also changed "+strings.Join(same, ", ")+".", now+": publishing replaces their change.")
	} else {
		text = append(text, now+".")
	}
	a.reviewOn(w, r, b, c, back, same, Alert{Kind: "warning", Text: strings.Join(text, " ")})
}

func latestText(n *int) string {
	if n == nil {
		return "no version"
	}
	return strconv.Itoa(*n)
}

// changedMeanwhile are the rules of the change that differ between the valid version the
// admin started from and the newer one.
func (a *App) changedMeanwhile(ctx context.Context, b base, c change, started *int, versions *api.ConfigVersions) ([]string, error) {
	old := json.RawMessage(emptyDoc)
	if started != nil {
		from := -1
		for _, v := range versions.Versions {
			if v.Valid && v.Number <= *started && v.Number > from {
				from = v.Number
			}
		}
		if from >= 0 {
			v, err := a.Service.ConfigVersion(ctx, from)
			if err != nil {
				return nil, err
			}
			old = v.Doc
		}
	}
	diff, err := diffRules(old, b.Doc)
	if err != nil {
		return nil, err
	}
	ids := c.touches()
	var out []string
	for _, group := range [][]ruleChange{diff.Added, diff.Removed, diff.Changed} {
		for _, rc := range group {
			if (ids == nil || slices.Contains(ids, rc.ID)) && !slices.Contains(out, rc.ID) {
				out = append(out, rc.ID)
			}
		}
	}
	return out, nil
}
