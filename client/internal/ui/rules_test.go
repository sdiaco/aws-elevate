package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

// fakeAdmin answers the AWS reads of the admin and records publications.
type fakeAdmin struct {
	refreshed bool
	published []string // documents
	latest    []*int
	conflict  bool
	previewed []string // versions
}

func (f *fakeAdmin) PermissionSets(_ context.Context, refresh bool) ([]awsx.Named, error) {
	f.refreshed = f.refreshed || refresh
	return []awsx.Named{{ID: "arn:aws:sso:::permissionSet/ssoins-1/ps-1", Name: "ssm-write"}, {ID: "arn:aws:sso:::permissionSet/ssoins-1/ps-9", Name: "db-admin"}}, nil
}
func (f *fakeAdmin) Accounts(context.Context) ([]awsx.Named, error) {
	return []awsx.Named{{ID: "111111111111", Name: "dev"}, {ID: "222222222222", Name: "prod"}}, nil
}
func (f *fakeAdmin) Groups(context.Context) ([]awsx.Named, error) {
	return []awsx.Named{{ID: "g-dev", Name: "devs"}, {ID: "g-lead", Name: "leads"}}, nil
}
func (f *fakeAdmin) PublishRules(_ context.Context, latest *int, doc []byte) (int, error) {
	if f.conflict {
		return 0, awsx.ErrVersionConflict
	}
	f.published = append(f.published, string(doc))
	f.latest = append(f.latest, latest)
	return 5, nil
}

func (f *fakeService) Rules(context.Context) (*api.RuleList, error) {
	return &api.RuleList{ConfigVersion: 3, Rules: f.rules}, nil
}

func (f *fakeService) CheckRules(_ context.Context, doc []byte) (*api.CheckResult, error) {
	f.calls = append(f.calls, "check")
	if strings.Contains(string(doc), `"broken"`) {
		id := "broken"
		return &api.CheckResult{Errors: []api.CheckError{{RuleID: &id, Code: "limits", Message: "ssm-write: inline policy too long"}}}, nil
	}
	return &api.CheckResult{Valid: true}, nil
}

// rulesHarness: an admin with version 3 in use (ssm-prod on a permission set that no
// longer exists, and ssm-dev) and version 4 not valid.
func rulesHarness() (*harness, *fakeAdmin) {
	h := newHarness("admin")
	three := 3
	h.svc.versions = &api.ConfigVersions{Current: &three, Versions: []api.VersionInfo{{Number: 4}, {Number: 3, Valid: true}}}
	h.svc.docs = map[int]string{3: `{"schemaVersion":1,"rules":[` +
		`{"id":"ssm-prod","group":{"id":"g-dev","name":"devs"},"permissionSet":{"arn":"arn:aws:sso:::permissionSet/ssoins-1/ps-gone","name":"old-ps"},"accounts":[{"id":"222222222222","name":"prod"}],"duration":{"defaultSeconds":3600,"maxSeconds":14400},"approval":"manual","approverGroup":{"id":"g-lead","name":"leads"},"privileged":true},` +
		`{"id":"ssm-dev","group":{"id":"g-dev","name":"devs"},"permissionSet":{"arn":"arn:aws:sso:::permissionSet/ssoins-1/ps-1","name":"ssm-write"},"accounts":[{"id":"111111111111","name":"dev"}],"duration":{"defaultSeconds":3600,"maxSeconds":7200},"approval":"auto","approverGroup":null,"privileged":false}]}`}
	adm := &fakeAdmin{}
	h.app.Admin = adm
	return h, adm
}

func TestRulesViewIsForAdminsWithAWSAccess(t *testing.T) {
	for _, role := range []string{"requester", "auditor", "approver"} {
		if w := newHarness(role).do(t, "GET", "/rules", nil); w.Code != 404 {
			t.Errorf("%s: %d", role, w.Code)
		}
	}
	h, _ := rulesHarness()
	contains(t, h.get(t, "/active"), `href="/rules"`)
}

func TestRulesListMarksAPermissionSetNotFound(t *testing.T) {
	h, _ := rulesHarness()
	out := h.get(t, "/rules")
	contains(t, h.get(t, "/rules/edit?id=ssm-prod"), "old-ps is no longer in Identity Center")
	contains(t, out, "Rules", `href="/rules/edit?id=ssm-prod"`, "old-ps", "PS not found", "privileged",
		"prod (222222222222)", "1h (max 4h)", "manual by leads", "auto",
		`href="/rules/edit"`, "New rule", `href="/rules/import"`, `formaction="/rules/delete"`)
	if strings.Count(out, "PS not found") != 1 {
		t.Errorf("only ssm-prod lost its permission set:\n%s", out)
	}
}

func TestRuleFormIsFilledFromTheRuleAndAWS(t *testing.T) {
	h, adm := rulesHarness()
	out := h.get(t, "/rules/edit?id=ssm-dev")
	contains(t, out,
		`<input type="hidden" name="orig" value="ssm-dev">`,
		`name="id" value="ssm-dev"`,
		`<option value="g-dev" selected>devs</option>`,
		`name="ps" value="ssm-write" list="ps-names"`, `<option value="db-admin">`,
		`name="account" value="111111111111" checked`, `name="account" value="222222222222"`,
		`name="default" value="1h"`, `name="max" value="2h"`,
		`name="approval" value="auto" checked`,
		`data-load="/rules/ps-permissions?ps=" data-load-from="ps"`,
	)
	lacks(t, out, `name="privileged" checked`)
	h.get(t, "/rules/edit?refresh=1")
	if !adm.refreshed {
		t.Error("refresh did not read AWS again")
	}
}

func TestNewRuleFormShowsWhatThePermissionSetContains(t *testing.T) {
	h, _ := rulesHarness()
	contains(t, h.get(t, "/rules/edit"), `data-load="/rules/ps-permissions?ps=" data-load-from="ps"`)
	lacks(t, h.get(t, "/rules/edit"), "shows once the rule is saved")
}

func TestPermissionSetContentsAreReadByTheServiceFromTheChosenName(t *testing.T) {
	h, _ := rulesHarness()
	out := h.get(t, "/rules/ps-permissions?ps=db-admin")
	contains(t, out, "AmazonSSMReadOnlyAccess", "ssm:PutParameter")
	if !slices.Contains(h.svc.calls, "ps permissions arn:aws:sso:::permissionSet/ssoins-1/ps-9") {
		t.Errorf("calls %v", h.svc.calls)
	}
	out = h.get(t, "/rules/ps-permissions?ps=nope")
	contains(t, out, "no permission set nope in Identity Center")
	n := 0
	for _, c := range h.svc.calls {
		if strings.HasPrefix(c, "ps permissions ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("an unknown name reached the service: %v", h.svc.calls)
	}
}

func postedRule(id string) url.Values {
	return url.Values{
		"orig": {""}, "id": {id}, "group": {"g-dev"}, "ps": {"db-admin"},
		"account": {"111111111111", "222222222222"}, "default": {"30m"}, "max": {"2h"},
		"approval": {"manual"}, "approver": {"g-lead"}, "privileged": {"on"},
	}
}

func hidden(out, name string) string {
	i := strings.Index(out, `name="`+name+`" value="`)
	if i < 0 {
		return ""
	}
	rest := out[i+len(`name="`+name+`" value="`):]
	v := rest[:strings.Index(rest, `"`)]
	return strings.NewReplacer("&#34;", `"`, "&lt;", "<", "&gt;", ">", "&amp;", "&", "&#39;", "'", "&#43;", "+").Replace(v)
}

func TestSavingARuleShowsTheChangesThenPublishesFromTheNewestVersion(t *testing.T) {
	h, adm := rulesHarness()
	w := h.do(t, "POST", "/rules/edit", postedRule("db-prod"))
	out := w.Body.String()
	contains(t, out, "Review changes", "Added", "db-prod", `formaction="/rules/publish"`, "Publish")
	if len(adm.published) != 0 || h.svc.calls[len(h.svc.calls)-1] != "check" {
		t.Fatalf("published before confirmation, calls %v", h.svc.calls)
	}
	w = h.do(t, "POST", "/rules/publish", url.Values{"change": {hidden(out, "change")}, "latest": {hidden(out, "latest")}})
	contains(t, w.Body.String(), `class="alert success"`, "Published rules version 5", "about 30 seconds")
	if len(adm.published) != 1 || *adm.latest[0] != 4 {
		t.Fatalf("published %v latest %v", adm.published, adm.latest)
	}
	var doc struct {
		SchemaVersion int        `json:"schemaVersion"`
		Rules         []api.Rule `json:"rules"`
	}
	if err := json.Unmarshal([]byte(adm.published[0]), &doc); err != nil {
		t.Fatal(err)
	}
	r := doc.Rules[2]
	if doc.SchemaVersion != 1 || len(doc.Rules) != 3 || r.ID != "db-prod" ||
		r.PermissionSet != (api.PermissionSet{Arn: "arn:aws:sso:::permissionSet/ssoins-1/ps-9", Name: "db-admin"}) ||
		r.Group != (api.Ref{ID: "g-dev", Name: "devs"}) || len(r.Accounts) != 2 || r.Accounts[1] != (api.Ref{ID: "222222222222", Name: "prod"}) ||
		r.Duration != (api.Duration{DefaultSeconds: 1800, MaxSeconds: 7200}) || r.Approval != "manual" ||
		r.ApproverGroup == nil || *r.ApproverGroup != (api.Ref{ID: "g-lead", Name: "leads"}) || !r.Privileged {
		t.Errorf("rule %+v", doc.Rules)
	}
}

func TestEditingKeepsTheOtherRulesAndRenamesInPlace(t *testing.T) {
	h, adm := rulesHarness()
	form := postedRule("ssm-dev-2")
	form.Set("orig", "ssm-dev")
	form.Set("approval", "auto")
	form.Del("approver")
	form.Del("privileged")
	out := h.do(t, "POST", "/rules/edit", form).Body.String()
	contains(t, out, "Added", "ssm-dev-2", "Removed", "ssm-dev")
	h.do(t, "POST", "/rules/publish", url.Values{"change": {hidden(out, "change")}, "latest": {hidden(out, "latest")}})
	var doc struct{ Rules []api.Rule }
	json.Unmarshal([]byte(adm.published[0]), &doc)
	if len(doc.Rules) != 2 || doc.Rules[0].ID != "ssm-prod" || doc.Rules[1].ID != "ssm-dev-2" || doc.Rules[1].ApproverGroup != nil {
		t.Errorf("rules %+v", doc.Rules)
	}
}

func TestServiceErrorsKeepTheFormFilled(t *testing.T) {
	h, adm := rulesHarness()
	out := h.do(t, "POST", "/rules/edit", postedRule("broken")).Body.String()
	contains(t, out, `class="alert error"`, "ssm-write: inline policy too long", `name="id" value="broken"`, `name="default" value="30m"`)
	form := postedRule("x")
	form.Set("default", "soon")
	form.Set("ps", "nope")
	out = h.do(t, "POST", "/rules/edit", form).Body.String()
	contains(t, out, "No permission set named nope", "Durations look like 45m, 1h or 1h30m")
	if len(adm.published) != 0 {
		t.Error("published")
	}
}

func TestConflictReviewsAgainAgainstTheNewVersion(t *testing.T) {
	h, adm := rulesHarness()
	out := h.do(t, "POST", "/rules/edit", url.Values{
		"orig": {"ssm-dev"}, "id": {"ssm-dev"}, "group": {"g-dev"}, "ps": {"ssm-write"}, "account": {"111111111111"},
		"default": {"1h"}, "max": {"3h"}, "approval": {"auto"},
	}).Body.String()
	change := hidden(out, "change")
	// Someone published version 5 meanwhile and changed ssm-dev too.
	author := "other@example.com"
	five := 5
	h.svc.versions = &api.ConfigVersions{Current: &five, Versions: []api.VersionInfo{{Number: 5, Valid: true, Author: &author}, {Number: 4}, {Number: 3, Valid: true}}}
	h.svc.docs[5] = strings.Replace(h.svc.docs[3], `"maxSeconds":7200`, `"maxSeconds":10800,"x":1`, 1)
	adm.conflict = true
	out = h.do(t, "POST", "/rules/publish", url.Values{"change": {change}, "latest": {"4"}}).Body.String()
	contains(t, out, "Review changes", "Version 5 was published by other@example.com after you started",
		"also changed ssm-dev: publishing replaces their change")
	if hidden(out, "latest") != "5" {
		t.Errorf("latest %q", hidden(out, "latest"))
	}
	adm.conflict = false
	h.do(t, "POST", "/rules/publish", url.Values{"change": {hidden(out, "change")}, "latest": {"5"}})
	if len(adm.published) != 1 || *adm.latest[0] != 5 {
		t.Errorf("published %v", adm.latest)
	}
}

func TestDeletingRules(t *testing.T) {
	h, adm := rulesHarness()
	out := h.do(t, "POST", "/rules/delete", url.Values{"id": {"ssm-prod"}}).Body.String()
	contains(t, out, "Review changes", "Removed", "ssm-prod")
	h.do(t, "POST", "/rules/publish", url.Values{"change": {hidden(out, "change")}, "latest": {hidden(out, "latest")}})
	if len(adm.published) != 1 || strings.Contains(adm.published[0], "ssm-prod") || !strings.Contains(adm.published[0], "ssm-dev") {
		t.Errorf("published %v", adm.published)
	}
}

func TestImportingADocumentReplacesTheRules(t *testing.T) {
	h, adm := rulesHarness()
	contains(t, h.get(t, "/rules/import"), `enctype="multipart/form-data"`, `type="file" name="doc"`)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("doc", "rules.json")
	fw.Write([]byte(`{"schemaVersion":1,"rules":[{"id":"only-rule"}]}`))
	mw.Close()
	r := httptest.NewRequest("POST", "/rules/import", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	Pages(h.app).ServeHTTP(w, r)
	out := w.Body.String()
	contains(t, out, "Review changes", "Added", "only-rule", "Removed", "ssm-prod", "ssm-dev")
	h.do(t, "POST", "/rules/publish", url.Values{"change": {hidden(out, "change")}, "latest": {hidden(out, "latest")}})
	if len(adm.published) != 1 || adm.published[0] != `{"schemaVersion":1,"rules":[{"id":"only-rule"}]}` {
		t.Errorf("published %v", adm.published)
	}
}
