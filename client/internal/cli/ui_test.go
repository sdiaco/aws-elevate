package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
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

func TestUIPrintsTheURLWhenTheBrowserDoesNotOpen(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.env.OpenBrowser = func(string) error { return errors.New("no browser") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stop right after start

	if code := Run(ctx, h.env, []string{"ui", "--profile", "work"}); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	out := h.out.String()
	if !strings.Contains(out, "http://127.0.0.1:") || !strings.Contains(out, "Could not open a browser: open this URL yourself (it works once): http://127.0.0.1:") {
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

func TestUIAdminNeverOffersTheServiceAccount(t *testing.T) {
	// A grant in the account running aws-elevate could tamper with it; the
	// service refuses it too, the form just does not list it.
	h := newHarness(t)
	adm := &fakeAdmin{accounts: []awsx.Named{{ID: "111111111111", Name: "shared-services"}, {ID: "222222222222", Name: "prod"}}}
	a := newUIAdmin(h.env, "work", adm, &api.Installation{StackID: "arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1"})
	got, err := a.Accounts(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "222222222222" {
		t.Errorf("%v %v", got, err)
	}
}

func TestUIAdminPublishesOnTheInstallationAndCachesPermissionSetsPerProfile(t *testing.T) {
	h := newHarness(t)
	h.env.CacheDir = t.TempDir()
	adm := &fakeAdmin{publishN: 7, permissionSets: []awsx.Named{{ID: "arn:ps-1", Name: "ssm-write"}}}
	a := newUIAdmin(h.env, "work", adm, &api.Installation{RulesApplicationID: "app1", RulesProfileID: "prof1"})
	ctx := context.Background()
	for range 2 {
		if sets, err := a.PermissionSets(ctx, false); err != nil || len(sets) != 1 {
			t.Fatalf("%v %v", sets, err)
		}
	}
	if adm.psReads != 1 {
		t.Errorf("AWS read %d times", adm.psReads)
	}
	if _, err := os.Stat(filepath.Join(h.env.CacheDir, "work", "permission-sets.json")); err != nil {
		t.Error(err)
	}
	latest := 3
	if n, err := a.PublishRules(ctx, &latest, []byte(`{}`)); err != nil || n != 7 || adm.published[0].app != "app1" || adm.published[0].profile != "prof1" {
		t.Errorf("%d %v %+v", n, err, adm.published)
	}

	h.env.CacheDir = ""
	a = newUIAdmin(h.env, "work", adm, &api.Installation{})
	a.PermissionSets(ctx, false)
	if adm.psReads != 2 {
		t.Errorf("without a cache directory AWS is read every time: %d", adm.psReads)
	}
}

func TestUISaysWhyItClosedAndHowToReopen(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.env.OpenBrowser = func(string) error { return nil }
	defer func(d time.Duration) { uiIdle = d }(uiIdle)
	uiIdle = 50 * time.Millisecond
	if code := Run(context.Background(), h.env, []string{"ui", "--profile", "work"}); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	want := "Closed after 30 minutes without use. Open it again with:\n  aws elevate ui --profile work\n"
	if !strings.HasSuffix(h.out.String(), want) {
		t.Errorf("stdout %q", h.out.String())
	}
}

func TestOpenerFailureIsReported(t *testing.T) {
	if err := runOpener(exec.Command("sh", "-c", "exit 3"), time.Second); err == nil {
		t.Fatal("a failing opener was reported as opened")
	}
}

func TestOpenerStillRunningCountsAsOpened(t *testing.T) {
	start := time.Now()
	if err := runOpener(exec.Command("sh", "-c", "sleep 5"), 100*time.Millisecond); err != nil {
		t.Fatalf("running opener: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("waited %s", time.Since(start))
	}
}

func TestOpenerThatExitsCleanlyCountsAsOpened(t *testing.T) {
	if err := runOpener(exec.Command("sh", "-c", "exit 0"), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestOpenerThatCannotStartIsReported(t *testing.T) {
	if err := runOpener(exec.Command("/nonexistent/opener"), time.Second); err == nil {
		t.Fatal("missing opener was reported as opened")
	}
}
