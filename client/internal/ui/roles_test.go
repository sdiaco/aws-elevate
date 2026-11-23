package ui

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

func countCalls(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

// The first rule makes an admin a requester: the menu and the routes follow without a
// restart, reading the roles again at most every rolesTTL.
func TestRolesRefreshAfterTheTTL(t *testing.T) {
	h := newHarness("admin")
	if w := h.do(t, "GET", "/requests/new", nil); w.Code != http.StatusNotFound {
		t.Fatalf("before: %d", w.Code)
	}
	h.svc.me = &api.Me{User: me, Roles: []string{"admin", "requester"}}

	// Within the TTL the roles are not read again.
	if w := h.do(t, "GET", "/requests/new", nil); w.Code != http.StatusNotFound {
		t.Fatalf("within the TTL: %d", w.Code)
	}
	if n := countCalls(h.svc.calls, "me"); n != 0 {
		t.Fatalf("me read %d times within the TTL", n)
	}

	h.app.rolesRead = time.Now().Add(-rolesTTL - time.Second)
	contains(t, h.get(t, "/requests"), `href="/requests/new"`)
	h.get(t, "/requests/new")
	if n := countCalls(h.svc.calls, "me"); n != 1 {
		t.Fatalf("me read %d times, want 1", n)
	}
	if !slices.Contains(h.app.me().Roles, "requester") {
		t.Fatalf("roles not updated: %v", h.app.me().Roles)
	}
}

func TestRolesRefreshFailureKeepsTheRolesAndSaysSo(t *testing.T) {
	h := newHarness("admin")
	h.svc.meErr = errors.New("ExpiredToken")
	h.app.rolesRead = time.Now().Add(-rolesTTL - time.Second)

	out := h.get(t, "/requests")
	contains(t, out, "Could not refresh your roles", "ExpiredToken")
	if got := h.app.me().Roles; !slices.Equal(got, []string{"admin"}) {
		t.Fatalf("roles changed: %v", got)
	}
}

// An expired SSO session is not a roles problem: the page says what to run, wherever the
// error comes from.
func TestExpiredSessionSaysWhatToRun(t *testing.T) {
	expired := fmt.Errorf("aws credentials: %w", &awsx.SessionExpiredError{Profile: "work", Err: errors.New("token expired")})
	want := []string{`class="alert error"`, "Your AWS session for work has expired.", `data-copy="aws sso login --profile work"`}

	h := newHarness("admin")
	h.svc.meErr = expired
	h.app.rolesRead = time.Now().Add(-rolesTTL - time.Second)
	out := h.get(t, "/requests")
	contains(t, out, want...)
	if strings.Contains(out, "Could not refresh your roles") {
		t.Fatal("expired session shown as a roles problem")
	}

	h = newHarness("requester")
	h.svc.rulesErr = expired
	contains(t, h.do(t, "GET", "/requests/new", nil).Body.String(), want...)
}

// A role comes from the published rules: once the service uses them, the menu must show
// it without waiting for rolesTTL.
func TestRolesAreReadAgainOnceTheRulesAreInUse(t *testing.T) {
	h, _ := rulesHarness()
	h.svc.versions = &api.ConfigVersions{Versions: []api.VersionInfo{{Number: 3, Valid: true}}}
	contains(t, h.get(t, "/rules"), "data-reload")
	h.svc.me = &api.Me{User: me, Roles: []string{"admin", "requester"}}

	three := 3
	h.svc.versions = &api.ConfigVersions{Current: &three, Versions: []api.VersionInfo{{Number: 3, Valid: true}}}
	lacks(t, h.get(t, "/rules"), "data-reload")
	if n := countCalls(h.svc.calls, "me"); n != 0 {
		t.Fatalf("me read %d times before the next page", n)
	}

	contains(t, h.get(t, "/requests"), `href="/requests/new"`)
	if n := countCalls(h.svc.calls, "me"); n != 1 {
		t.Fatalf("me read %d times, want 1", n)
	}
	// Only the change from publishing to in use expires the roles.
	h.get(t, "/rules")
	h.get(t, "/requests")
	if n := countCalls(h.svc.calls, "me"); n != 1 {
		t.Fatalf("me read %d times, want still 1", n)
	}
}
