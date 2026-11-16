package ui

import (
	"net/url"
	"testing"
)

func TestFirstRunOpensWhileNoRuleExists(t *testing.T) {
	h, _ := rulesHarness()
	h.svc.rules = nil
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
	h, _ := rulesHarness()
	h.svc.rules = nil
	if w := h.do(t, "POST", "/welcome/skip", url.Values{}); w.Code != 303 {
		t.Fatalf("skip: %d", w.Code)
	}
	if w := h.do(t, "GET", "/", nil); w.Header().Get("Location") == "/welcome" {
		t.Fatal("home went back to the first run after skipping")
	}
}

func TestFirstRunIsForAdminsOnly(t *testing.T) {
	h := newHarness("requester")
	h.svc.rules = nil
	if w := h.do(t, "GET", "/welcome", nil); w.Code != 404 {
		t.Fatalf("/welcome as requester: %d", w.Code)
	}
	if w := h.do(t, "GET", "/", nil); w.Header().Get("Location") != "/request" {
		t.Fatalf("home as requester: %q", w.Header().Get("Location"))
	}
}
