package ui

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"
)

// files are the pages, styles and script, inside the executable (design §11.1).
//
//go:embed static templates
var files embed.FS

// Page is what the base layout shows around a view.
type Page struct {
	Title   string
	Path    string // the current view, marked in the navigation
	Nav     []NavGroup
	User    string
	Roles   string
	Alerts  []Alert
	Content any  // the data of the view's "content" template
	Bare    bool // full screen, without the navigation
}

// Alert is a message above the view. Kind is error, success, warning or info.
type Alert struct {
	Kind string
	Text string
}

type NavGroup struct {
	Label string // empty for the first group
	Items []NavItem
}

type NavItem struct {
	Label string
	Path  string
	Count int
	Warn  bool
}

// List is the data of the one list template: header with title, counter and actions,
// search, filters, table.
type List struct {
	Title   string
	Search  bool
	Filters []FilterGroup
	Columns []string
	Rows    []Row
	Actions []Action // with actions the rows can be selected; disabled until some are
	Tools   []Action // header links that need no selection
	Empty   string
	First   string // address of the first page, when this is not it
	Next    string // address of the next page
}

// FilterGroup is one segmented control; its label names it for screen readers.
type FilterGroup struct {
	Label string
	Items []Filter
}

type Filter struct {
	Label string
	Path  string
	On    bool
}

type Row struct {
	ID      string   // the value sent by the actions; empty for a row that cannot be selected
	Actions []string // the named actions this row allows
	State   string   // the grant state, so the page sees when a polled row moves on
	Poll    string   // address of the row's fresh HTML while its state can still change
	Cells   []Cell
}

// rowCtx renders one row inside its table: with or without the selection column.
type rowCtx struct {
	Row   Row
	Check bool
}

// Cell shows the fields that are set, in this order: state, text or link, tags, copyable
// id, countdown, relative time.
type Cell struct {
	State    string
	Text     string
	Href     string
	External bool // opens in a new tab, without telling the other site where it came from
	Muted    bool
	Tags     []string
	Copy     string
	Until    time.Time
	At       time.Time
}

// Action is a header button. Destructive actions are normal buttons, as in the console;
// the others are primary. A named action is enabled only when every selected row allows
// it; a tool (a link that needs no selection) is primary only when asked.
type Action struct {
	Label   string
	Path    string
	Name    string
	Danger  bool
	Primary bool
}

// stateLabels name the states of contract/vocabulary.json on screen.
var stateLabels = map[string]string{
	"pending":       "Pending",
	"activating":    "Activating",
	"active":        "Active",
	"revoking":      "Revoking",
	"revoke_failed": "Revoke failed",
	"closed":        "Closed",
}

var funcs = template.FuncMap{
	"stateLabel": func(s string) string { return stateLabels[s] },
	"iso":        func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"exact":      func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
	"left":       left,
	"ago":        ago,
	"short":      short,
	"can":        can,
	"has":        func(list []string, s string) bool { return slices.Contains(list, s) },
	"indent":     indent,
	"join":       strings.Join,
	"tags":       tags,
	"row":        func(r Row, check bool) rowCtx { return rowCtx{r, check} },
	"columns": func(l List) int {
		if len(l.Actions) > 0 {
			return len(l.Columns) + 1
		}
		return len(l.Columns)
	},
}

// parts are the shared templates; templates holds one set per view: the base layout, the
// shared parts and the view.
var (
	parts     = template.Must(template.New("").Funcs(funcs).ParseFS(files, "templates/*.html"))
	templates = parseTemplates()
)

func parseTemplates() map[string]*template.Template {
	shared := parts
	views, err := fs.Glob(files, "templates/views/*.html")
	if err != nil {
		panic(err)
	}
	sets := map[string]*template.Template{}
	for _, v := range views {
		set := template.Must(template.Must(shared.Clone()).ParseFS(files, v))
		sets[strings.TrimSuffix(path.Base(v), ".html")] = set
	}
	return sets
}

// renderPage writes the whole page or, on a template error, nothing.
func renderPage(w http.ResponseWriter, status int, view string, p Page) error {
	set, ok := templates[view]
	if !ok {
		return fmt.Errorf("no view %q", view)
	}
	return write(w, status, set, "base", p)
}

// renderPart writes a shared template alone: the HTML a page fetches into itself.
func renderPart(w http.ResponseWriter, name string, data any) error {
	return write(w, http.StatusOK, parts, name, data)
}

func write(w http.ResponseWriter, status int, set *template.Template, name string, data any) error {
	var b bytes.Buffer
	if err := set.ExecuteTemplate(&b, name, data); err != nil {
		return fmt.Errorf("render %s: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := b.WriteTo(w)
	return err
}

// indent lays out JSON for reading; anything unreadable is shown as it came.
func indent(raw []byte) string {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return string(raw)
	}
	return b.String()
}

// short is a duration as the CLI takes it: 45m, 1h, 1h30m.
func short(seconds int) string {
	h, m, sec := seconds/3600, seconds%3600/60, seconds%60
	out := ""
	if h > 0 {
		out += fmt.Sprintf("%dh", h)
	}
	if m > 0 {
		out += fmt.Sprintf("%dm", m)
	}
	if sec > 0 || out == "" {
		out += fmt.Sprintf("%ds", sec)
	}
	return out
}

// left is the countdown text, the same app.js updates every second.
func left(until time.Time) string {
	s := int(time.Until(until).Round(time.Second).Seconds())
	switch {
	case s <= 0:
		return "0 min"
	case s < 60:
		return fmt.Sprintf("%d s", s)
	}
	m := (s + 59) / 60
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

func ago(at time.Time) string {
	d := time.Since(at)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}
