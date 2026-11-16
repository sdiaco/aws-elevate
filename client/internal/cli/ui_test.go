package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

func TestUIOpensTheBrowserOnALocalServerUntilInterrupted(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	opened := make(chan string, 1)
	h.env.OpenBrowser = func(url string) error {
		opened <- url
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Run(ctx, h.env, []string{"ui", "--profile", "work"}) }()

	var url string
	select {
	case url = <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("browser not opened")
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("url %q", url)
	}
	jar, _ := cookiejar.New(nil)
	resp, err := (&http.Client{Jar: jar}).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "aws-elevate") {
		t.Errorf("%d %q", resp.StatusCode, body)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d, stderr %s", code, h.errOut)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still running")
	}
}

func TestUIPrintsTheAddressWhenNoBrowserOpens(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.env.OpenBrowser = func(string) error { return errors.New("no browser") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stop right after start

	if code := Run(ctx, h.env, []string{"ui", "--profile", "work"}); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	out := h.out.String()
	if !strings.Contains(out, "http://127.0.0.1:") || !strings.Contains(out, "Open this address") {
		t.Errorf("output %q", out)
	}
}

func TestUINeedsSetup(t *testing.T) {
	h := newHarness(t)
	h.env.OpenBrowser = func(string) error { t.Error("browser opened"); return nil }
	if code := h.run("ui", "--profile", "work"); code != 1 || !strings.Contains(h.errOut.String(), "setup") {
		t.Errorf("exit %d, stderr %s", code, h.errOut)
	}
}
