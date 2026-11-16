package ui

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var page = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "page "+r.Method)
})

func newTestServer(t *testing.T, idle time.Duration) *Server {
	t.Helper()
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s, err := New(ln, page, idle)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// do sends one request straight to the handler, as the browser would to s.host.
func do(s *Server, method, target string, header map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = s.host
	for k, v := range header {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// login exchanges the start URL for the session cookie.
func login(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	u, err := url.Parse(s.StartURL())
	if err != nil {
		t.Fatal(err)
	}
	w := do(s, "GET", u.RequestURI(), nil, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("start: %d %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies %v", cookies)
	}
	return cookies[0]
}

func TestListensOnLoopbackOnARandomPort(t *testing.T) {
	s := newTestServer(t, time.Minute)
	host, port, err := net.SplitHostPort(s.host)
	if err != nil || host != "127.0.0.1" || port == "0" {
		t.Errorf("host %q", s.host)
	}
	if !strings.HasPrefix(s.StartURL(), "http://"+s.host+"/start?k=") {
		t.Errorf("start URL %q", s.StartURL())
	}
}

func TestTheStartURLGivesAnHttpOnlyStrictCookieOnce(t *testing.T) {
	s := newTestServer(t, time.Minute)
	c := login(t, s)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Errorf("cookie %+v", c)
	}
	if strings.Contains(s.StartURL(), c.Value) {
		t.Error("the cookie is the secret of the start URL")
	}
	u, _ := url.Parse(s.StartURL())
	if w := do(s, "GET", u.RequestURI(), nil, nil); w.Code != http.StatusForbidden {
		t.Errorf("second use of the start URL: %d", w.Code)
	}
	if w := do(s, "GET", "/", nil, c); w.Code != http.StatusOK || w.Body.String() != "page GET" {
		t.Errorf("page: %d %q", w.Code, w.Body)
	}
}

func TestAWrongStartSecretIsRefused(t *testing.T) {
	s := newTestServer(t, time.Minute)
	for _, target := range []string{"/start", "/start?k=", "/start?k=00"} {
		if w := do(s, "GET", target, nil, nil); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
	login(t, s) // a wrong secret does not burn the right one
}

func TestRequestsAreRefused(t *testing.T) {
	s := newTestServer(t, time.Minute)
	c := login(t, s)
	own := "http://" + s.host
	cases := []struct {
		name   string
		method string
		header map[string]string
		cookie *http.Cookie
	}{
		{"without cookie", "GET", nil, nil},
		{"with a wrong cookie", "GET", nil, &http.Cookie{Name: c.Name, Value: "x"}},
		{"from another origin", "GET", map[string]string{"Origin": "http://evil.example"}, c},
		{"from another port", "POST", map[string]string{"Origin": "http://127.0.0.1:1"}, c},
		{"with another host", "GET", map[string]string{"Host": "evil.example:80"}, c},
		{"with localhost as host", "GET", map[string]string{"Host": strings.Replace(s.host, "127.0.0.1", "localhost", 1)}, c},
		{"a write without origin", "POST", nil, c},
		{"start with another host", "GET", map[string]string{"Host": "evil.example"}, nil},
	}
	for _, tc := range cases {
		target := "/"
		if strings.HasPrefix(tc.name, "start") {
			u, _ := url.Parse(s.StartURL())
			target = u.RequestURI()
		}
		w := do(s, tc.method, target, tc.header, tc.cookie)
		if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "page") {
			t.Errorf("%s: %d %q", tc.name, w.Code, w.Body)
		}
	}
	if w := do(s, "POST", "/", map[string]string{"Origin": own}, c); w.Code != http.StatusOK {
		t.Errorf("write from the own origin: %d", w.Code)
	}
}

func TestResponsesAllowNoCrossOriginUseAndNoExternalResource(t *testing.T) {
	s := newTestServer(t, time.Minute)
	c := login(t, s)
	for _, w := range []*httptest.ResponseRecorder{do(s, "GET", "/", nil, c), do(s, "GET", "/", nil, nil)} {
		h := w.Header()
		if h.Get("Access-Control-Allow-Origin") != "" {
			t.Error("CORS header set")
		}
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP %q lacks %q", csp, want)
			}
		}
		if h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("headers %v", h)
		}
	}
}

func TestServeStopsAfterInactivity(t *testing.T) {
	s := newTestServer(t, 200*time.Millisecond)
	c := login(t, s)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.Serve(context.Background()) }()
	time.Sleep(120 * time.Millisecond)
	do(s, "GET", "/", nil, c) // activity: the 200 ms start again
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
		if d := time.Since(start); d < 300*time.Millisecond {
			t.Errorf("stopped after %v, activity ignored", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still serving")
	}
}

func TestServeStopsWithTheContextAndAnswersOverTheNetwork(t *testing.T) {
	s := newTestServer(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()

	jar := &cookieJar{}
	client := &http.Client{Jar: jar}
	resp, err := client.Get(s.StartURL())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "page GET" {
		t.Errorf("%d %q", resp.StatusCode, body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still serving")
	}
}

// cookieJar keeps the cookies of one host, enough for the redirect after /start.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, c []*http.Cookie) { j.cookies = append(j.cookies, c...) }
func (j *cookieJar) Cookies(*url.URL) []*http.Cookie         { return j.cookies }
