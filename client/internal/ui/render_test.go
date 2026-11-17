package ui

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The pages load nothing from elsewhere (design §8.6) and the CSP forbids inline code.
func TestEmbeddedFilesLoadNothingFromOutsideAndHaveNoInlineCode(t *testing.T) {
	outside := regexp.MustCompile(`https?://|(src|href|action)="//|url\(|@import`)
	inline := regexp.MustCompile(`<script>|<script [^>]*>[^<]|<style|\sstyle=|\son[a-z]+=|javascript:`)
	n := 0
	err := fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		b, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		if m := outside.Find(b); m != nil {
			t.Errorf("%s loads from outside: %q", path, m)
		}
		if strings.HasSuffix(path, ".html") {
			if m := inline.Find(b); m != nil {
				t.Errorf("%s has inline code: %q", path, m)
			}
		}
		return nil
	})
	if err != nil || n < 4 {
		t.Fatalf("walked %d files: %v", n, err)
	}
}

func TestEveryStateHasALabelAndAColor(t *testing.T) {
	raw, err := os.ReadFile("../../../contract/vocabulary.json")
	if err != nil {
		t.Fatal(err)
	}
	var vocab struct{ States []string }
	if err := json.Unmarshal(raw, &vocab); err != nil {
		t.Fatal(err)
	}
	css, _ := files.ReadFile("static/app.css")
	// "starting" is shown for a pending request the service cannot decide yet; the rest
	// are the Status of a rule, which is no grant state.
	shown := append(vocab.States, "starting", "publishing", "deleting", "ps_missing")
	for _, s := range shown {
		if stateLabels[s] == "" {
			t.Errorf("no label for state %s", s)
		}
		if !bytes.Contains(css, []byte(".st-"+s+" {")) {
			t.Errorf("no color for state %s", s)
		}
	}
	if len(stateLabels) != len(shown) {
		t.Errorf("labels for %d states, %d shown", len(stateLabels), len(shown))
	}
}

func render(t *testing.T, view string, p Page) string {
	t.Helper()
	w := httptest.NewRecorder()
	if err := renderPage(w, http.StatusOK, view, p); err != nil {
		t.Fatal(err)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	return w.Body.String()
}

func TestBaseShowsTheNavigationAndTheCurrentView(t *testing.T) {
	out := render(t, "message", Page{
		Title: "Active", Path: "/active", User: "user@example.com", Roles: "admin",
		Nav: []NavGroup{
			{Items: []NavItem{{Label: "Request", Path: "/request"}, {Label: "Active", Path: "/active", Count: 5}}},
			{Label: "Admin", Items: []NavItem{{Label: "Issues", Path: "/issues", Count: 1, Warn: true}}},
		},
	})
	for _, want := range []string{
		`<title>Active · aws-elevate</title>`,
		`<link rel="stylesheet" href="/static/app.css">`,
		`<a href="/active" aria-current="page">Active <span class="count">5</span></a>`,
		`<a href="/request">Request</a>`,
		`<div class="nav-group">Admin</div>`,
		`<span class="count warn">1</span>`,
		`user@example.com · admin`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}

func TestListWithActionsHasSelectionAndEveryKindOfCell(t *testing.T) {
	until := time.Now().Add(38 * time.Minute)
	at := time.Now().Add(-4 * time.Minute)
	l := List{
		Title:   "Active",
		Search:  true,
		Filters: []FilterGroup{{Items: []Filter{{Label: "All (2)", Path: "/active", On: true}, {Label: "Mine (1)", Path: "/active?mine=1"}}}},
		Columns: []string{"State", "Rule", "Expires in", "Requested", "Id"},
		Rows: []Row{{ID: "k3f9x2m7qa", Cells: []Cell{
			{State: "active"},
			{Text: "ssm-write", Href: "/rules/ssm-write", Tags: []string{"removed"}},
			{Until: until},
			{At: at},
			{Copy: "k3f9x2m7qa"},
		}}},
		Actions: []Action{{Label: "Revoke…", Path: "/revoke", Danger: true}},
	}
	out := renderList(t, l)
	for _, want := range []string{
		`<form class="list" data-list method="get">`,
		`<div class="list-head"><h1>Active <span class="counter">(1)</span></h1>`,
		`<input type="search" data-search`,
		`<div class="segmented" role="group" aria-label="Filter"><a href="/active" aria-current="true">All (2)</a>`,
		`<a href="/active?mine=1">Mine (1)</a>`,
		`data-select-all`,
		`<input type="checkbox" name="id" value="k3f9x2m7qa" data-select`,
		`<span class="st st-active">Active</span>`,
		`<a href="/rules/ssm-write">ssm-write</a> <span class="tag">removed</span>`,
		`<time data-until datetime="` + until.UTC().Format(time.RFC3339) + `" title="` + until.Local().Format("2006-01-02 15:04:05") + `">38 min</time>`,
		`>4 min ago</time>`,
		`<button type="button" class="copy" data-copy="k3f9x2m7qa"`,
		`<button class="btn" formaction="/revoke" disabled data-action="">Revoke…<span data-selected-count></span></button>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}

func TestListWithoutActionsOrRows(t *testing.T) {
	out := renderList(t, List{Title: "History", Columns: []string{"When", "Who"}, Empty: "No grants in this period.", Next: "/history?page=x"})
	if strings.Contains(out, "checkbox") || strings.Contains(out, "data-action") || strings.Contains(out, "data-search") {
		t.Errorf("selection or search without actions:\n%s", out)
	}
	for _, want := range []string{
		`<td class="empty" colspan="2">No grants in this period.</td>`,
		`<a class="btn" href="/history?page=x">Next page</a>`,
		`<h1>History <span class="counter">(0+)</span></h1>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	withActions := renderList(t, List{Columns: []string{"When", "Who"}, Actions: []Action{{Label: "Approve", Path: "/approve"}}})
	if !strings.Contains(withActions, `colspan="3"`) {
		t.Errorf("the empty row does not span the selection column:\n%s", withActions)
	}
	if !strings.Contains(withActions, `<button class="btn primary" formaction="/approve" disabled data-action="">`) {
		t.Errorf("a non-destructive action is not the primary button:\n%s", withActions)
	}
}

func renderList(t *testing.T, l List) string {
	t.Helper()
	var b bytes.Buffer
	if err := parts.ExecuteTemplate(&b, "list", l); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestTimesAreRelative(t *testing.T) {
	now := time.Now()
	for d, want := range map[time.Duration]string{
		30 * time.Second: "30 s", 38 * time.Minute: "38 min", 90 * time.Minute: "1 h 30 min", -time.Minute: "0 min",
	} {
		if got := left(now.Add(d)); got != want {
			t.Errorf("left %v: %q, want %q", d, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 4 * time.Minute: "4 min ago", 3 * time.Hour: "3 h ago", 50 * time.Hour: "2 days ago",
	} {
		if got := ago(now.Add(-d)); got != want {
			t.Errorf("ago %v: %q, want %q", d, got, want)
		}
	}
}

func TestStaticFilesAreServed(t *testing.T) {
	h := Pages(&App{})
	for path, ct := range map[string]string{"/static/app.css": "text/css", "/static/app.js": "text/javascript"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), ct) {
			t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
}

func TestDurationsAreShortAsTheCLITakesThem(t *testing.T) {
	for s, want := range map[int]string{3600: "1h", 2700: "45m", 5400: "1h30m", 30: "30s", 0: "0s", 14400: "4h"} {
		if got := short(s); got != want {
			t.Errorf("short(%d) = %q, want %q", s, got, want)
		}
	}
}
