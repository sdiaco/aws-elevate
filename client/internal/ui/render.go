package ui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
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
	Content any // the data of the view's "content" template
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
	Filters []Filter
	Columns []string
	Rows    []Row
	Actions []Action // with actions the rows can be selected; disabled until some are
	Empty   string
	Next    string // address of the next page
}

type Filter struct {
	Label string
	Path  string
	On    bool
}

type Row struct {
	ID    string // the value sent by the actions; empty for a row that cannot be selected
	Cells []Cell
}

// Cell shows the fields that are set, in this order: state, text or link, tags, copyable
// id, countdown, relative time.
type Cell struct {
	State string
	Text  string
	Href  string
	Muted bool
	Tags  []string
	Copy  string
	Until time.Time
	At    time.Time
}

// Action is a header button. Destructive actions are normal buttons, as in the console;
// the others are primary.
type Action struct {
	Label  string
	Path   string
	Danger bool
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
	"columns": func(l List) int {
		if len(l.Actions) > 0 {
			return len(l.Columns) + 1
		}
		return len(l.Columns)
	},
}

// templates holds one set per view: the base layout, the shared parts and the view.
var templates = parseTemplates()

func parseTemplates() map[string]*template.Template {
	shared := template.Must(template.New("").Funcs(funcs).ParseFS(files, "templates/*.html"))
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
func renderPage(w http.ResponseWriter, view string, p Page) error {
	set, ok := templates[view]
	if !ok {
		return fmt.Errorf("no view %q", view)
	}
	var b bytes.Buffer
	if err := set.ExecuteTemplate(&b, "base", p); err != nil {
		return fmt.Errorf("render %s: %w", view, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := b.WriteTo(w)
	return err
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
