package ui

import (
	"net/url"
	"strings"
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
		"Your first rule", "First activation", "Step 1", "Step 5", "Review",
		"1. What", "2. Where", "3. Who", "4. How",
		`<input type="hidden" name="welcome" value="1">`,
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
	if w := h.do(t, "GET", "/", nil); w.Header().Get("Location") != "/requests" {
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

var firstRule = url.Values{
	"welcome": {"1"}, "id": {"ssm-dev"}, "group": {"g-dev"}, "ps": {"ssm-write"}, "account": {"111111111111"},
	"default": {"1h"}, "max": {"3h"}, "approval": {"manual"}, "approver": {"g-lead"},
}

func TestFirstRunReviewIsItsLastStepInReadableForm(t *testing.T) {
	h := freshInstall()
	out := h.do(t, "POST", "/rules/edit", firstRule).Body.String()
	contains(t, out,
		"Your first rule", "First activation", "5. Review", `data-step-review`,
		// what, where, who and how, each with its way back to the step
		"ssm-write", "dev", "111111111111", "devs", "Manual · leads", "1h", "3h",
		`data-step-goto="0"`, `data-step-goto="1"`, `data-step-goto="2"`, `data-step-goto="3"`,
		"Show JSON", `&#34;id&#34;: &#34;ssm-dev&#34;`,
		`formaction="/rules/publish"`, ">Publish</button>",
	)
	if hidden(out, "change") == "" {
		t.Error("no change to publish")
	}
	// the steps keep what was chosen, for Edit
	contains(t, out, `value="ssm-dev"`, `value="3h"`)
	lacks(t, out, `class="nav"`, "<h1>Review changes</h1>", "In use", "After publishing")
}

func TestFirstRunRefusedByTheServiceStaysInTheWizard(t *testing.T) {
	h := freshInstall()
	form := url.Values{}
	for k, v := range firstRule {
		form[k] = v
	}
	form.Set("id", "broken")
	out := h.do(t, "POST", "/rules/edit", form).Body.String()
	contains(t, out, "Your first rule", "Rule broken: ssm-write: inline policy too long", `value="broken"`)
	lacks(t, out, `class="nav"`, "data-step-review")
	if strings.Contains(out, ">Publish</button>") {
		t.Error("offers to publish a refused rule")
	}
}
