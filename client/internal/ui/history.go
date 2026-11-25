package ui

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

type periodChoice struct {
	key, label string
	from       func(time.Time) time.Time
}

// periods on screen: 30 days by default, up to the months the service keeps. The longest
// is addressed as "all", so a link stays valid when the retention changes.
func (a *App) periods() []periodChoice {
	months := api.HistoryMonths(a.me())
	return []periodChoice{
		{"30d", "30 days", func(t time.Time) time.Time { return t.AddDate(0, 0, -30) }},
		{"90d", "90 days", func(t time.Time) time.Time { return t.AddDate(0, 0, -90) }},
		{"all", strconv.Itoa(months) + " months", func(t time.Time) time.Time { return t.AddDate(0, -months, 0) }},
	}
}

// historyQuery reads period and scope from the address; unknown values fall back to the
// defaults, and "all" only for the roles the service gives it to. Who never requests
// starts on "all": their own history is always empty.
func (a *App) historyQuery(r *http.Request) (period, scope string, q api.ListQuery) {
	periods := a.periods()
	period, scope = periods[0].key, "mine"
	scopeWanted := r.URL.Query().Get("scope")
	if scopeWanted == "" && !a.has("requester") {
		scopeWanted = "all"
	}
	from := periods[0].from
	want := r.URL.Query().Get("period")
	if want == "13m" { // address of the longest period before it was called "all"
		want = "all"
	}
	for _, p := range periods {
		if p.key == want {
			period, from = p.key, p.from
		}
	}
	if scopeWanted == "all" && a.seesAll() {
		scope = "all"
	}
	return period, scope, api.ListQuery{Scope: scope, Closed: true, From: from(time.Now()).UTC().Format(time.RFC3339)}
}

func (a *App) seesAll() bool { return a.has("admin") || a.has("auditor") }

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
		Columns: []string{"Id", "Outcome", "Requested", "Rule", "Account"},
		Tools:   []Action{{Label: "Export CSV", Path: "/history/export.csv?" + base}},
	}
	if a.seesAll() {
		l.Filters = append(l.Filters, FilterGroup{Label: "Whose", Items: []Filter{
			{Label: "Mine", Path: "/history?period=" + period + "&scope=mine", On: scope == "mine"},
			{Label: "Everyone", Path: "/history?period=" + period + "&scope=all", On: scope == "all"},
		}})
	}
	group := FilterGroup{Label: "Period"}
	for _, p := range a.periods() {
		group.Items = append(group.Items, Filter{Label: p.label, Path: "/history?period=" + p.key + "&scope=" + scope, On: p.key == period})
	}
	l.Filters = append(l.Filters, group)
	// Someone else's rows name their requester; the user's own would only repeat it.
	if scope == "all" {
		l.Columns = slices.Insert(l.Columns, 3, "Requester")
	}
	for _, g := range page.Items {
		cells := []Cell{
			{Text: g.ID, Href: "/history?" + here + "&open=" + g.ID, Panel: "/panel/" + g.ID + "?from=history"},
			{Outcome: closeReasonOf(g)},
			{At: g.CreatedAt},
			{Text: g.RuleID, Tags: tags(g)},
			accountCell(g),
		}
		if scope == "all" {
			cells = slices.Insert(cells, 3, Cell{Text: g.Requester.UserName})
		}
		l.Rows = append(l.Rows, Row{Key: g.ID, Cells: cells})
	}
	if page.Cursor != nil {
		l.Next = "/history?" + base + "&cursor=" + *page.Cursor
	}
	if cursor != "" {
		l.First = "/history?" + base
	}
	v := listView{List: l}
	// The detail is read only when opened.
	if id := r.URL.Query().Get("open"); id != "" {
		p, err := a.panel(r.Context(), id, "/history?"+here)
		if err != nil {
			v.Alert = new(errorAlert(err))
		} else {
			v.Panel.Panel = p
			markOpen(v.List.Rows, id)
		}
	}
	a.show(w, http.StatusOK, "listpanel", "History", "/history", v)
}

var csvHeader = []string{
	"id", "state", "closeReason", "ruleId", "permissionSet", "account", "accountName", "requester", "approver",
	"privileged", "durationSeconds", "reason", "createdAt", "activatedAt", "expiresAt", "closedAt",
	"revokedBy", "revokeReason", "rejectReason", "error", "configVersion",
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
		g.ID, g.State, str(g.CloseReason), g.RuleID, g.SourcePs.Name, g.Account, g.AccountName, g.Requester.UserName,
		user(g.Approver), strconv.FormatBool(g.Privileged), strconv.Itoa(g.DurationSeconds), g.Reason,
		at(&g.CreatedAt), at(g.ActivatedAt), at(g.ExpiresAt), at(g.ClosedAt), user(g.RevokedBy),
		str(g.RevokeReason), str(g.RejectReason), str(g.Error), strconv.Itoa(g.ConfigVersion),
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

// closeReasonOf is the close reason of a closed grant, or "".
func closeReasonOf(g api.Grant) string {
	if g.CloseReason == nil {
		return ""
	}
	return *g.CloseReason
}
