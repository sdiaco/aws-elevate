package ui

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
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
