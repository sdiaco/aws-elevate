package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// versionsPage lists the rules versions with author and validity (design §7.3).
func (a *App) versionsPage(w http.ResponseWriter, r *http.Request) {
	v, err := a.Service.ConfigVersions(r.Context())
	if err != nil {
		a.fail(w, "Config versions", "/versions", err)
		return
	}
	l := List{
		Title: "Config versions", Search: true, Empty: "No rules version published yet.",
		Columns: []string{"Version", "Author", "Published", "Validity", ""},
	}
	for _, info := range v.Versions {
		n := strconv.Itoa(info.Number)
		author := Cell{Text: "author being verified", Muted: true}
		if info.Author != nil {
			author = Cell{Text: *info.Author}
		}
		published := Cell{}
		if info.CreatedAt != nil {
			published.At = *info.CreatedAt
		}
		validity := Cell{Text: "valid"}
		if !info.Valid {
			validity = Cell{Text: "not valid, ignored", Tags: info.Errors}
		}
		var tags []string
		if v.Current != nil && *v.Current == info.Number {
			tags = []string{"in use"}
		}
		l.Rows = append(l.Rows, Row{ID: n, Cells: []Cell{
			{Text: "Version " + n, Href: "/versions/" + n, Tags: tags}, author, published, validity,
			{Text: "Export", Href: "/versions/" + n + "/export.json"},
		}})
	}
	if len(l.Rows) > 1 {
		l.Actions = []Action{{Label: "Compare", Path: "/versions/compare"}}
	}
	a.show(w, http.StatusOK, "list", "Config versions", "/versions", l)
}

type versionView struct {
	Info api.VersionInfo
	Doc  string
}

func (a *App) version(w http.ResponseWriter, r *http.Request) (*api.ConfigVersion, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	v, err := a.Service.ConfigVersion(r.Context(), n)
	if err != nil {
		a.fail(w, "Config versions", "/versions", err)
		return nil, false
	}
	return v, true
}

func (a *App) versionPage(w http.ResponseWriter, r *http.Request) {
	v, ok := a.version(w, r)
	if !ok {
		return
	}
	title := fmt.Sprintf("Version %d", v.Version.Number)
	a.show(w, http.StatusOK, "version", title, "/versions", versionView{Info: v.Version, Doc: indent(v.Doc)})
}

// versionExport downloads a version as a file that admin import takes back (design §7.4).
func (a *App) versionExport(w http.ResponseWriter, r *http.Request) {
	v, ok := a.version(w, r)
	if !ok {
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

// markLines marks the lines of each side that are not in their longest common
// subsequence: what was removed on the left, what was added on the right.
func markLines(before, after string) ([]line, []line) {
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
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
			Alert{Kind: "error", Text: err.Error()})
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
			view.Added = append(view.Added, ruleChange{ID: id, After: indent(after.rules[id])})
		case !jsonEqual(b, after.rules[id]):
			rc := ruleChange{ID: id, Before: indent(b), After: indent(after.rules[id])}
			rc.BeforeLines, rc.AfterLines = markLines(rc.Before, rc.After)
			view.Changed = append(view.Changed, rc)
		}
	}
	for _, id := range before.order {
		if _, ok := after.rules[id]; !ok {
			view.Removed = append(view.Removed, ruleChange{ID: id, Before: indent(before.rules[id])})
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
