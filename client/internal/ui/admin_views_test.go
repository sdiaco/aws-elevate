package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

func (f *fakeService) Redrive(_ context.Context, id string) (*api.Grant, error) {
	return f.act("redrive", id)
}

func (f *fakeService) ConfigVersions(context.Context) (*api.ConfigVersions, error) {
	f.calls = append(f.calls, "versions")
	return f.versions, nil
}

func (f *fakeService) ConfigVersion(_ context.Context, n int) (*api.ConfigVersion, error) {
	f.calls = append(f.calls, "version "+string(rune('0'+n)))
	doc, ok := f.docs[n]
	if !ok {
		return nil, &api.Error{Status: 404, Code: "not_found", Message: "no version"}
	}
	for _, v := range f.versions.Versions {
		if v.Number == n {
			return &api.ConfigVersion{Version: v, Doc: json.RawMessage(doc)}, nil
		}
	}
	return nil, &api.Error{Status: 404, Code: "not_found", Message: "no version"}
}

func TestAdminViewsFollowTheRoles(t *testing.T) {
	for path, allowed := range map[string][]string{
		"/versions": {"admin", "auditor"},
	} {
		for _, role := range []string{"requester", "approver", "admin", "auditor"} {
			h := newHarness(role)
			h.svc.versions = &api.ConfigVersions{}
			w := h.do(t, "GET", path, nil)
			want := 404
			for _, a := range allowed {
				if a == role {
					want = 200
				}
			}
			if w.Code != want {
				t.Errorf("%s as %s: %d, want %d", path, role, w.Code, want)
			}
		}
	}
	contains(t, newHarness("admin").get(t, "/requests"), "Administration", `href="/versions"`)
	contains(t, newHarness("auditor").get(t, "/requests"), `href="/versions"`)
	lacks(t, newHarness("requester").get(t, "/requests"), "Administration")
}

func failedRevoke(id string, actions ...string) api.Grant {
	g := grant(id, api.StateRevokeFailed, other, actions...)
	msg := "DeleteAccountAssignment: AccessDenied"
	g.Error = &msg
	return g
}

func versionsFixture(h *harness) {
	author := "admin@example.com"
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	three := 3
	h.svc.versions = &api.ConfigVersions{Current: &three, Versions: []api.VersionInfo{
		{Number: 4, Valid: false, Errors: []string{"schema: approval must be auto or manual"}},
		{Number: 3, Valid: true, Author: &author, CreatedAt: &at, Verified: true},
		{Number: 2, Valid: true, CreatedAt: &at},
	}}
	h.svc.docs = map[int]string{
		2: `{"schemaVersion":1,"rules":[{"id":"ssm-dev","privileged":false},{"id":"old-rule"}]}`,
		3: `{"schemaVersion":1,"rules":[{"id":"ssm-dev","privileged":true},{"id":"ssm-prod"}]}`,
	}
}

func TestVersionsListAuthorsValidityAndTheVersionInUse(t *testing.T) {
	h := newHarness("auditor")
	versionsFixture(h)
	out := h.get(t, "/versions")
	contains(t, out,
		"admin@example.com", "author being verified", "in use",
		"schema: approval must be auto or manual", "not valid, ignored",
		`href="/versions/3"`, `formaction="/versions/compare"`, "Compare",
		`href="/versions/3/export.json"`,
	)
}

func TestVersionShowsTheDocumentAndExportsIt(t *testing.T) {
	h := newHarness("admin")
	versionsFixture(h)
	out := h.get(t, "/versions/3")
	contains(t, out, "Version 3", "&#34;ssm-prod&#34;", `href="/versions/3/export.json"`)

	w := h.do(t, "GET", "/versions/3/export.json", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), `attachment; filename="elevate-rules-v3.json"`) {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if !strings.Contains(w.Body.String(), "\n  \"rules\": [") {
		t.Errorf("not indented: %s", w.Body)
	}
	if w := h.do(t, "GET", "/versions/x", nil); w.Code != 404 {
		t.Errorf("bad number: %d", w.Code)
	}
}

func TestCompareTwoVersionsRuleByRule(t *testing.T) {
	h := newHarness("admin")
	versionsFixture(h)
	out := h.get(t, "/versions/compare?id=3&id=2")
	contains(t, out, "Version 2 → 3",
		"Added", "ssm-prod",
		"Removed", "old-rule",
		"Changed", "ssm-dev", `<span class="del">  &#34;privileged&#34;: false</span>`, `<span class="add">  &#34;privileged&#34;: true</span>`,
		`<span>  &#34;id&#34;: &#34;ssm-dev&#34;,</span>`,
	)
	out = h.get(t, "/versions/compare?id=3")
	contains(t, out, `class="alert error"`, "Select two versions")
}
