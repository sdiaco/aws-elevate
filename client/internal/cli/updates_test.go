package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/release"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// The harness runs client 0.0.1 against infrastructure 0.0.1.
var releases = []release.Release{
	{Version: "0.2.0", Type: "feature", Name: "New engine", URL: "https://example.com/v0.2.0", MinInfra: "0.2.0", MinClient: "0.2.0"},
	{Version: "0.1.0", Type: "security", Name: "Tighter api role", URL: "https://example.com/v0.1.0", MinInfra: "0.0.1", MinClient: "0.0.1"},
	{Version: "0.0.1", Type: "feature", Name: "First", MinInfra: "0.0.1", MinClient: "0.0.1"},
}

// withReleases also pins the running client to 0.0.1, so the fake releases stay newer
// whatever the version of the code under test.
func (h *harness) withReleases(t *testing.T, rels []release.Release) *int {
	old := version.Client
	version.Client = "0.0.1"
	t.Cleanup(func() { version.Client = old })
	calls := 0
	h.env.Releases = func(context.Context) ([]release.Release, error) {
		calls++
		return rels, nil
	}
	return &calls
}

func TestUpdatesListsClientAndInfrastructureUpdates(t *testing.T) {
	h := adminHarness(t)
	h.withReleases(t, releases)
	if code := h.run("admin", "updates", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	out := h.out.String()
	client, infra, ok := strings.Cut(out, "Infrastructure")
	if !ok {
		t.Fatalf("no infrastructure section:\n%s", out)
	}
	// the 0.2.0 client needs infrastructure 0.2.0: not proposed.
	if !strings.Contains(client, "0.1.0") || strings.Contains(client, "0.2.0") || !strings.Contains(client, "aws-elevate update") {
		t.Errorf("client updates:\n%s", client)
	}
	for _, want := range []string{"0.2.0", "feature", "New engine", "needs client 0.2.0", "0.1.0", "security", "admin updates --preview 0.2.0"} {
		if !strings.Contains(infra, want) {
			t.Errorf("infrastructure updates lack %q:\n%s", want, infra)
		}
	}
}

func TestUpdatesUpToDate(t *testing.T) {
	h := adminHarness(t)
	h.withReleases(t, releases[2:])
	if code := h.run("admin", "updates", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if !strings.Contains(h.out.String(), "up to date") {
		t.Errorf("out:\n%s", h.out)
	}
}

func TestUpdatesCheckCanBeTurnedOff(t *testing.T) {
	h := adminHarness(t)
	calls := h.withReleases(t, releases)
	settings, _ := awsx.LoadSettings(h.env.SettingsPath)
	settings.CheckUpdates = ptr(false)
	if err := awsx.SaveSettings(h.env.SettingsPath, settings); err != nil {
		t.Fatal(err)
	}
	if code := h.run("admin", "updates", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if *calls != 0 || !strings.Contains(h.errOut.String(), "checkUpdates") {
		t.Errorf("calls %d, stderr %s", *calls, h.errOut)
	}
	// a preview reads no release list: it still works.
	if code := h.run("admin", "updates", "--profile", "work", "--preview", "0.2.0"); code != 0 {
		t.Errorf("preview exit %d, stderr %s", code, h.errOut)
	}
}

func TestUpdatesFailWhenTheReleasesCannotBeRead(t *testing.T) {
	h := adminHarness(t)
	h.env.Releases = func(context.Context) ([]release.Release, error) {
		return nil, errors.New("read the releases: GitHub answered 403")
	}
	if code := h.run("admin", "updates", "--profile", "work"); code != 1 || !strings.Contains(h.errOut.String(), "403") {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
}

func TestPreviewShowsTheChangesAndTheCommandToApply(t *testing.T) {
	h := adminHarness(t)
	calls := h.withReleases(t, releases)
	admin := h.backend.admin
	admin.preview = awsx.Preview{
		Parameters: []string{"AdminGroupId", "AlertEmail", "RequesterGroupId"},
		Changes: []awsx.Change{
			{Action: "Modify", Resource: "EngineFunction", Type: "AWS::Lambda::Function", Replacement: "False"},
			{Action: "Add", Resource: "NewQueue", Type: "AWS::SQS::Queue"},
		},
	}
	if code := h.run("admin", "updates", "--profile", "work", "--preview", "0.2.0"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	wantURL := "https://aws-elevate-releases-eu-west-1.s3.eu-west-1.amazonaws.com/v0.2.0/elevate.yaml"
	if admin.previewed[0] != installation.StackID || admin.previewed[1] != wantURL {
		t.Errorf("previewed %v", admin.previewed)
	}
	if *calls != 0 {
		t.Errorf("preview read the release list")
	}
	out := h.out.String()
	for _, want := range []string{
		"Modify", "EngineFunction", "AWS::Lambda::Function", "Add", "NewQueue",
		"aws cloudformation update-stack", "--region eu-west-1", "--stack-name elevate", "--template-url " + wantURL,
		"ParameterKey=AdminGroupId,UsePreviousValue=true", "ParameterKey=RequesterGroupId,UsePreviousValue=true",
		"--capabilities CAPABILITY_NAMED_IAM", "<your admin profile>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestPreviewWithoutChanges(t *testing.T) {
	h := adminHarness(t)
	h.backend.admin.preview = awsx.Preview{NoChanges: true}
	if code := h.run("admin", "updates", "--profile", "work", "--preview", "0.0.1"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if !strings.Contains(h.out.String(), "No changes") || strings.Contains(h.out.String(), "update-stack") {
		t.Errorf("out:\n%s", h.out)
	}
}
