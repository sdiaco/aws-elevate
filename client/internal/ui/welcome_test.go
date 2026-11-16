package ui

import (
	"net/url"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// freshInstall: no rules version was ever published, so GET /rules answers 503
// config_unavailable and the version list is empty (seen in the test organization).
func freshInstall() *harness {
	h, _ := rulesHarness()
	h.svc.versions = &api.ConfigVersions{}
	h.svc.rules = nil
	h.svc.rulesErr = &api.Error{Status: 503, Code: api.CodeConfigUnavailable, Message: "no valid rules version"}
	return h
}

func TestFirstRunOpensWhileNoRuleExists(t *testing.T) {
	h := freshInstall()
	w := h.do(t, "GET", "/", nil)
	if w.Code != 303 || w.Header().Get("Location") != "/welcome" {
		t.Fatalf("home: %d to %q, want /welcome", w.Code, w.Header().Get("Location"))
	}
	out := h.get(t, "/welcome")
	contains(t, out,
		"Your first rule", "1. What", "2. Where", "3. Who", "4. How",
		`action="/rules/edit"`, `name="ps"`, `name="account"`, `name="group"`, `name="approval"`, `name="default"`,
		`data-step`, `action="/welcome/skip"`, "Skip",
	)
	lacks(t, out, `class="nav"`)
}

func TestFirstRunIsGoneOnceARuleExists(t *testing.T) {
	h, _ := rulesHarness()
	if w := h.do(t, "GET", "/", nil); w.Header().Get("Location") == "/welcome" {
		t.Fatal("home went to the first run with a rule in place")
	}
	if w := h.do(t, "GET", "/welcome", nil); w.Code != 303 || w.Header().Get("Location") != "/rules" {
		t.Fatalf("/welcome: %d to %q, want /rules", w.Code, w.Header().Get("Location"))
	}
}

func TestFirstRunCanBeSkipped(t *testing.T) {
	h := freshInstall()
	if w := h.do(t, "POST", "/welcome/skip", url.Values{}); w.Code != 303 {
		t.Fatalf("skip: %d", w.Code)
	}
	if w := h.do(t, "GET", "/", nil); w.Header().Get("Location") == "/welcome" {
		t.Fatal("home went back to the first run after skipping")
	}
}

func TestFirstRunIsForAdminsOnly(t *testing.T) {
	h := freshInstall()
	h.app.Me.Roles = []string{"requester"}
	if w := h.do(t, "GET", "/welcome", nil); w.Code != 404 {
		t.Fatalf("/welcome as requester: %d", w.Code)
	}
	if w := h.do(t, "GET", "/", nil); w.Header().Get("Location") != "/request" {
		t.Fatalf("home as requester: %q", w.Header().Get("Location"))
	}
}

func TestFirstRunOpensWhenTheVersionInUseHasNoRule(t *testing.T) {
	h, _ := rulesHarness()
	three := 3
	h.svc.versions = &api.ConfigVersions{Current: &three, Versions: []api.VersionInfo{{Number: 3, Valid: true}}}
	h.svc.docs = map[int]string{3: `{"schemaVersion":1,"rules":[]}`}
	if w := h.do(t, "GET", "/", nil); w.Header().Get("Location") != "/welcome" {
		t.Fatalf("home: %q, want /welcome", w.Header().Get("Location"))
	}
}
