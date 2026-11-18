// Package ui is the local web interface of aws-elevate. The pages
// call the service through this process, which holds the credentials: the browser
// never sees them.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Listen opens a random port on the loopback address only.
func Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open a local port for the UI: %w", err)
	}
	return ln, nil
}

// Server serves content to the one browser that opened the start URL.
type Server struct {
	ln      net.Listener
	host    string // "127.0.0.1:<port>", the only Host accepted
	content http.Handler
	idle    time.Duration

	mu      sync.Mutex
	secret  string // in the start URL
	used    bool   // the start URL works once
	session string // in the cookie
	active  chan struct{}
}

// New prepares a server on ln that closes after idle time without authenticated requests.
func New(ln net.Listener, content http.Handler, idle time.Duration) (*Server, error) {
	secret, err := token()
	if err != nil {
		return nil, err
	}
	session, err := token()
	if err != nil {
		return nil, err
	}
	return &Server{
		ln: ln, host: ln.Addr().String(), content: content, idle: idle,
		secret: secret, session: session, active: make(chan struct{}, 1),
	}, nil
}

func token() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate the UI secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// StartURL is the address to open in the browser. It works once.
func (s *Server) StartURL() string { return "http://" + s.host + "/start?k=" + s.secret }

// ErrIdle ends Serve when no authenticated request arrived for the idle time.
var ErrIdle = errors.New("closed without activity")

// Serve answers until ctx ends or no authenticated request arrives for the idle time,
// then returns nil or ErrIdle.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(s.ln) }()

	timer := time.NewTimer(s.idle)
	defer timer.Stop()
	var stopped error
	for waiting := true; waiting; {
		select {
		case <-s.active:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(s.idle)
		case <-ctx.Done():
			waiting = false
		case <-timer.C:
			waiting, stopped = false, ErrIdle
		case err := <-failed:
			return fmt.Errorf("UI server: %w", err)
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return fmt.Errorf("stop the UI server: %w", err)
	}
	if err := <-failed; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("UI server: %w", err)
	}
	return stopped
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	// The pages load nothing from elsewhere and may not be framed by another site.
	h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")

	// An exact Host defeats DNS rebinding; a foreign Origin is another site's page.
	if r.Host != s.host || !s.sameOrigin(r) {
		forbid(w)
		return
	}
	if r.URL.Path == "/start" {
		s.start(w, r)
		return
	}
	c, err := r.Cookie(s.cookieName())
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) != 1 {
		forbid(w)
		return
	}
	select {
	case s.active <- struct{}{}:
	default: // a signal is already waiting
	}
	if r.URL.Path == "/alive" { // the open page says it is still there
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.content.ServeHTTP(w, r)
}

// sameOrigin accepts reads without Origin (navigation) and writes only from this page.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	}
	return origin == "http://"+s.host
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ok := r.Method == http.MethodGet && !s.used &&
		subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("k")), []byte(s.secret)) == 1
	if ok {
		s.used = true // the URL is in the terminal and in the browser history: one use only
	}
	s.mu.Unlock()
	if !ok {
		forbid(w)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: s.session, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// cookieName carries the port: browsers share cookies across the ports of a host, so a
// second interface (another profile) would otherwise replace this one's session.
func (s *Server) cookieName() string {
	_, port, _ := strings.Cut(s.host, ":")
	return "elevate_session_" + port
}

func forbid(w http.ResponseWriter) {
	http.Error(w, "Forbidden: start the interface again with aws elevate ui", http.StatusForbidden)
}
