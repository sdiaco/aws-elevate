package ui

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// periods on screen (design §11.3): 30 days by default, up to the 13 months kept.
var periods = []struct {
	key, label string
	from       func(time.Time) time.Time
}{
	{"30d", "30 days", func(t time.Time) time.Time { return t.AddDate(0, 0, -30) }},
	{"90d", "90 days", func(t time.Time) time.Time { return t.AddDate(0, 0, -90) }},
	{"13m", "13 months", func(t time.Time) time.Time { return t.AddDate(0, -13, 0) }},
}

// historyQuery reads period and scope from the address; unknown values fall back to the
// defaults, and "all" only for the roles the service gives it to.
func (a *App) historyQuery(r *http.Request) (period, scope string, q api.ListQuery) {
	period, scope = periods[0].key, "mine"
	from := periods[0].from
	for _, p := range periods {
		if p.key == r.URL.Query().Get("period") {
			period, from = p.key, p.from
		}
	}
	if r.URL.Query().Get("scope") == "all" && a.seesAll() {
		scope = "all"
	}
	return period, scope, api.ListQuery{Scope: scope, From: from(time.Now()).UTC().Format(time.RFC3339)}
}

func (a *App) seesAll() bool { return a.has("admin") || a.has("auditor") }

type historyView struct {
	List   List
	Detail *api.GrantDetail
	Close  string
	Alert  *Alert
}

func (a *App) historyPage(w http.ResponseWriter, r *http.Request) {
	period, scope, q := a.historyQuery(r)
	cursor := r.URL.Query().Get("cursor")
	q.Cursor = cursor
	page, err := a.Service.Grants(r.Context(), q)
	if err != nil {
		a.fail(w, "History", "/history", err)
		return
	}
	base := "period=" + period + "&scope=" + scope
	here := base
	if cursor != "" {
		here += "&cursor=" + cursor
	}
	l := List{
		Title: "History", Search: true, Empty: "No grant in this period.",
		Columns: []string{"Requested", "Requester", "Permission set", "Account", "Outcome", "Id", ""},
		Tools:   []Action{{Label: "Export CSV", Path: "/history/export.csv?" + base}},
	}
	if a.seesAll() {
		l.Filters = append(l.Filters, FilterGroup{Label: "Whose", Items: []Filter{
			{Label: "Mine", Path: "/history?period=" + period + "&scope=mine", On: scope == "mine"},
			{Label: "Everyone", Path: "/history?period=" + period + "&scope=all", On: scope == "all"},
		}})
	}
	group := FilterGroup{Label: "Period"}
	for _, p := range periods {
		group.Items = append(group.Items, Filter{Label: p.label, Path: "/history?period=" + p.key + "&scope=" + scope, On: p.key == period})
	}
	l.Filters = append(l.Filters, group)
	for _, g := range page.Items {
		l.Rows = append(l.Rows, Row{Cells: []Cell{
			{At: g.CreatedAt},
			{Text: g.Requester.UserName},
			{Text: g.SourcePs.Name, Tags: tags(g)},
			{Text: g.Account},
			{State: g.State, Text: closeReason(g), Muted: true},
			{Copy: g.ID},
			{Text: "Details", Href: "/history?" + here + "&open=" + g.ID},
		}})
	}
	if page.Cursor != nil {
		l.Next = "/history?" + base + "&cursor=" + *page.Cursor
	}
	if cursor != "" {
		l.First = "/history?" + base
	}
	v := historyView{List: l, Close: "/history?" + here}
	// The detail is read only when opened (design §11.1).
	if id := r.URL.Query().Get("open"); id != "" {
		d, err := a.Service.Grant(r.Context(), id)
		if err != nil {
			v.Alert = &Alert{Kind: "error", Text: err.Error()}
		} else {
			v.Detail = d
		}
	}
	a.show(w, http.StatusOK, "history", "History", "/history", v)
}

var csvHeader = []string{
	"id", "state", "closeReason", "ruleId", "permissionSet", "account", "requester", "approver",
	"privileged", "durationSeconds", "reason", "createdAt", "activatedAt", "expiresAt", "closedAt",
	"revokedBy", "revokeReason", "error", "configVersion",
}

// historyExport writes every page of the period as CSV. It reads and encodes them all
// first, so a failure is an error page, not a silently shorter file.
func (a *App) historyExport(w http.ResponseWriter, r *http.Request) {
	_, _, q := a.historyQuery(r)
	grants, err := listAll(r.Context(), a.Service, q)
	if err != nil {
		http.Error(w, "Export failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	var b bytes.Buffer
	out := csv.NewWriter(&b)
	if err := out.Write(csvHeader); err != nil {
		http.Error(w, "Export failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, g := range grants {
		if err := out.Write(csvRow(g)); err != nil {
			http.Error(w, "Export failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	out.Flush()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="aws-elevate-history.csv"`)
	// A failure here means the browser went away: nobody is left to tell.
	_, _ = b.WriteTo(w)
}

func csvRow(g api.Grant) []string {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	user := func(u *api.User) string {
		if u == nil {
			return ""
		}
		return u.UserName
	}
	at := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	row := []string{
		g.ID, g.State, str(g.CloseReason), g.RuleID, g.SourcePs.Name, g.Account, g.Requester.UserName,
		user(g.Approver), strconv.FormatBool(g.Privileged), strconv.Itoa(g.DurationSeconds), g.Reason,
		at(&g.CreatedAt), at(g.ActivatedAt), at(g.ExpiresAt), at(g.ClosedAt), user(g.RevokedBy),
		str(g.RevokeReason), str(g.Error), strconv.Itoa(g.ConfigVersion),
	}
	for i, v := range row {
		row[i] = inert(v)
	}
	return row
}

// inert keeps a spreadsheet from running a value as a formula: reasons are free text
// typed by any requester (CSV injection).
func inert(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
