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
// when another version comes out meanwhile, it can be applied again to that version
// (design §7.1): put a rule (replacing Orig), delete rules, or replace the document.
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
			rules[i] = c.Rule
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
	// publishing them needs one more confirmation (design §7.2).
	Replaces []string
	// Deletes are the rules this publish removes; deleting asks once more in a dialog
	// (design §11.1).
	Deletes     []string
	DeleteTitle string
}

// review asks the service to check the changed document, then shows what changes and
// waits for confirmation (design §7.1: no drafts, summary and confirmation). On a check
// error, back shows the page the admin came from with the errors.
func (a *App) review(w http.ResponseWriter, r *http.Request, c change, back func(...Alert), alerts ...Alert) {
	b, err := a.base(r.Context())
	if err != nil {
		a.fail(w, "Rules", "/rules", err)
		return
	}
	a.reviewOn(w, r, b, c, back, nil, alerts...)
}

func (a *App) reviewOn(w http.ResponseWriter, r *http.Request, b base, c change, back func(...Alert), replaces []string, alerts ...Alert) {
	doc, err := c.apply(b.Doc)
	if err != nil {
		back(errorAlert(err))
		return
	}
	check, err := a.Service.CheckRules(r.Context(), doc)
	if err != nil {
		back(errorAlert(err))
		return
	}
	if !check.Valid {
		back(checkAlerts(check.Errors)...)
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

func checkAlerts(errs []api.CheckError) []Alert {
	out := make([]Alert, len(errs))
	for i, e := range errs {
		where := "Document"
		if e.RuleID != nil {
			where = "Rule " + *e.RuleID
		}
		out[i] = Alert{Kind: "error", Text: where + ": " + e.Message}
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
	text := fmt.Sprintf("Published rules version %d. It applies to new requests within about 30 seconds.", n)
	if b.Latest != nil && n <= *b.Latest {
		text = fmt.Sprintf("No change: version %d already has this content.", n)
	}
	a.redirectWithNotice(w, r, "/rules", notice{
		Kind: "success",
		Text: text + " The list shows it once the service has read it.",
	})
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
