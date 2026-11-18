package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// versionsPage lists the rules versions with author and validity (design §7.3); a
// version opens in the bottom panel.
func (a *App) versionsPage(w http.ResponseWriter, r *http.Request) {
	v, err := a.Service.ConfigVersions(r.Context())
	if err != nil {
		a.fail(w, "Config versions", "/versions", err)
		return
	}
	l := List{
		Title: "Config versions", Search: true, Empty: "No rules version published yet.",
		Columns: []string{"Version", "Author", "Published", "Validity"},
	}
	for _, info := range v.Versions {
		n := strconv.Itoa(info.Number)
		author := Cell{Text: "author being verified", Muted: true}
		if info.Author != nil {
			author = Cell{Text: userName(*info.Author), Title: *info.Author}
		}
		published := Cell{}
		if info.CreatedAt != nil {
			published.At = *info.CreatedAt
		}
		// The errors can be long: the list counts them, the panel shows them.
		validity := Cell{State: "valid"}
		if !info.Valid {
			validity = Cell{State: "invalid", Text: errorCount(len(info.Errors)) + ", ignored", Muted: true}
		}
		var tags []string
		if v.Current != nil && *v.Current == info.Number {
			tags = []string{"in use"}
		}
		l.Rows = append(l.Rows, Row{ID: n, Key: n, Cells: []Cell{
			{Text: "Version " + n, Href: "/versions?open=" + n, Panel: "/versions/" + n + "/panel", Tags: tags},
			author, published, validity,
		}})
	}
	if len(l.Rows) > 1 {
		l.Actions = []Action{{Label: "Compare", Path: "/versions/compare", Name: "compare", Count: 2}}
		for i := range l.Rows {
			l.Rows[i].Actions = []string{"compare"}
		}
	}
	view := listView{List: l}
	if open := r.URL.Query().Get("open"); open != "" {
		n, err := strconv.Atoi(open)
		if err == nil {
			view.Panel.Version, err = a.versionPanel(r.Context(), n, v.Current)
		}
		if err != nil {
			view.Alert = new(errorAlert(err))
		} else {
			markOpen(view.List.Rows, open)
		}
	}
	a.show(w, http.StatusOK, "listpanel", "Config versions", "/versions", view)
}

// versionPanel is one rules version at the bottom of Config versions.
type versionPanel struct {
	Info    api.VersionInfo
	Doc     string
	Current int // the version in use, 0 when none is
}

func (a *App) versionPanel(ctx context.Context, n int, current *int) (*versionPanel, error) {
	v, err := a.Service.ConfigVersion(ctx, n)
	if err != nil {
		return nil, err
	}
	p := &versionPanel{Info: v.Version, Doc: indent(v.Doc)}
	if current != nil {
		p.Current = *current
	}
	return p, nil
}

// versionPanelPart is the panel of one version alone, fetched when a row is chosen.
func (a *App) versionPanelPart(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	list, err := a.Service.ConfigVersions(r.Context())
	if err != nil {
		partError(w, err)
		return
	}
	p, err := a.versionPanel(r.Context(), n, list.Current)
	if err != nil {
		partError(w, err)
		return
	}
	part(w, "panel", panelSlot{Version: p})
}

// versionExport downloads a version as a file that admin import takes back (design §7.4).
func (a *App) versionExport(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	v, err := a.Service.ConfigVersion(r.Context(), n)
	if err != nil {
		a.fail(w, "Config versions", "/versions", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="elevate-rules-v%d.json"`, v.Version.Number))
	fmt.Fprintln(w, indent(v.Doc))
}

// ruleChange is one rule that differs between two versions; Before and After are its
// JSON, empty when the rule is not in that version.
type ruleChange struct {
	ID            string
	Before, After string
	// The two sides line by line, the lines that differ marked.
	BeforeLines, AfterLines []line
}

type line struct {
	Text    string
	Changed bool
}

// lines splits a text into its lines; an empty text has none, so a rule missing on one
// side shows as an empty column.
func lines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// markLines marks the lines of each side that are not in their longest common
// subsequence: what was removed on the left, what was added on the right.
func markLines(before, after string) ([]line, []line) {
	a, b := lines(before), lines(after)
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	left, right := make([]line, len(a)), make([]line, len(b))
	for i := range a {
		left[i] = line{Text: a[i], Changed: true}
	}
	for j := range b {
		right[j] = line{Text: b[j], Changed: true}
	}
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			left[i].Changed, right[j].Changed = false, false
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
	return left, right
}

type compareView struct {
	Old, New                int
	OldLabel, NewLabel      string
	Added, Removed, Changed []ruleChange
	Unchanged               int  // rules read and left as they are
	Other                   bool // a field outside the rules changed
}

func (a *App) compare(w http.ResponseWriter, r *http.Request) {
	var ns []int
	for _, id := range r.URL.Query()["id"] {
		n, err := strconv.Atoi(id)
		if err == nil && !slices.Contains(ns, n) {
			ns = append(ns, n)
		}
	}
	if len(ns) != 2 {
		a.show(w, http.StatusOK, "message", "Compare versions", "/versions", "Compare versions",
			Alert{Kind: "error", Text: "Select two versions to compare."})
		return
	}
	slices.Sort(ns)
	var docs [2]json.RawMessage
	for i, n := range ns {
		v, err := a.Service.ConfigVersion(r.Context(), n)
		if err != nil {
			a.fail(w, "Compare versions", "/versions", err)
			return
		}
		docs[i] = v.Doc
	}
	view, err := diffRules(docs[0], docs[1])
	if err != nil {
		a.show(w, http.StatusOK, "message", "Compare versions", "/versions", "Compare versions",
			errorAlert(err))
		return
	}
	view.Old, view.New = ns[0], ns[1]
	view.OldLabel, view.NewLabel = fmt.Sprintf("Version %d", ns[0]), fmt.Sprintf("Version %d", ns[1])
	a.show(w, http.StatusOK, "compare", "Compare versions", "/versions", view)
}

// diffRules compares two rules documents rule by rule, by id. It only lays out what
// changed: the service checks documents, the client does not.
func diffRules(old, new json.RawMessage) (compareView, error) {
	var view compareView
	before, restOld, err := rulesByID(old)
	if err != nil {
		return view, err
	}
	after, restNew, err := rulesByID(new)
	if err != nil {
		return view, err
	}
	for _, id := range after.order {
		b, ok := before.rules[id]
		switch {
		case !ok:
			rc := ruleChange{ID: id, After: indent(after.rules[id])}
			rc.BeforeLines, rc.AfterLines = markLines("", rc.After)
			view.Added = append(view.Added, rc)
		case !jsonEqual(b, after.rules[id]):
			rc := ruleChange{ID: id, Before: indent(b), After: indent(after.rules[id])}
			rc.BeforeLines, rc.AfterLines = markLines(rc.Before, rc.After)
			view.Changed = append(view.Changed, rc)
		default:
			view.Unchanged++
		}
	}
	for _, id := range before.order {
		if _, ok := after.rules[id]; !ok {
			rc := ruleChange{ID: id, Before: indent(before.rules[id])}
			rc.BeforeLines, rc.AfterLines = markLines(rc.Before, "")
			view.Removed = append(view.Removed, rc)
		}
	}
	view.Other = !jsonEqual(restOld, restNew)
	return view, nil
}

type ruleSet struct {
	order []string
	rules map[string]json.RawMessage
}

// rulesByID splits a document into its rules, keyed by id, and the rest of it.
func rulesByID(doc json.RawMessage) (ruleSet, json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return ruleSet{}, nil, fmt.Errorf("a version is not a JSON object: %w", err)
	}
	var list []json.RawMessage
	if raw, ok := top["rules"]; ok {
		if err := json.Unmarshal(raw, &list); err != nil {
			return ruleSet{}, nil, fmt.Errorf("the rules of a version are not a list: %w", err)
		}
	}
	delete(top, "rules")
	set := ruleSet{rules: map[string]json.RawMessage{}}
	for i, raw := range list {
		var head struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &head) // a rule without an id is shown under its position
		id := head.ID
		if id == "" {
			id = fmt.Sprintf("rule #%d", i+1)
		}
		set.order = append(set.order, id)
		set.rules[id] = raw
	}
	rest, err := json.Marshal(top)
	return set, rest, err
}

// jsonEqual compares two JSON values ignoring spacing and key order.
func jsonEqual(x, y json.RawMessage) bool {
	var a, b any
	if json.Unmarshal(x, &a) != nil || json.Unmarshal(y, &b) != nil {
		return bytes.Equal(x, y)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// userName is the last part of an identity ARN: the SSO user of an assumed role, or an IAM
// user. The column stays narrow; the full ARN shows on hover and in the panel.
func userName(arn string) string {
	return arn[strings.LastIndex(arn, "/")+1:]
}

func errorCount(n int) string {
	if n == 1 {
		return "1 error"
	}
	return strconv.Itoa(n) + " errors"
}

// AppConfig prints its whole JSON schema before the reason a version failed it; the
// reason is what an admin needs, so it comes first and the full text stays one click away.
const schemaReason = "Original error message:"

func errorReason(e string) string {
	if i := strings.Index(e, schemaReason); i >= 0 {
		return "schema: " + strings.TrimSpace(e[i+len(schemaReason):])
	}
	return e
}
