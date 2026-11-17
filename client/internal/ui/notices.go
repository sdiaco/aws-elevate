package ui

import (
	"crypto/rand"
	"net/http"
	"sync"
)

// notice is the outcome of an action, shown once at the top of the page the action
// returns to. It replaces a results page: the browser is redirected, so a reload does not
// send the action again.
type notice struct {
	Kind  string   // success, warning or error
	Text  string   // the summary line
	Items []string // one line per grant that did not go as asked
}

// notices hold each outcome until the page after the redirect shows it.
type notices struct {
	mu sync.Mutex
	m  map[string]notice
}

// put keeps n and returns the key the next page reads it with.
func (s *notices) put(n notice) string {
	key := rand.Text()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]notice{}
	}
	s.m[key] = n
	return key
}

// take returns the notice once: a second read of the same address shows nothing.
func (s *notices) take(key string) *notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.m[key]
	if !ok {
		return nil
	}
	delete(s.m, key)
	return &n
}

// redirectWithNotice sends the browser to path, which shows n at its top.
func (a *App) redirectWithNotice(w http.ResponseWriter, r *http.Request, path string, n notice) {
	http.Redirect(w, r, withParam(path, "notice", a.notices.put(n)), http.StatusSeeOther)
}

// noticeOf reads the notice the address names, if any.
func (a *App) noticeOf(r *http.Request) *notice {
	key := r.URL.Query().Get("notice")
	if key == "" {
		return nil
	}
	return a.notices.take(key)
}
