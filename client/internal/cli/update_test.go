package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/release"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// withExecutable gives the harness an installed executable and a download of any
// version, recorded in downloaded.
func (h *harness) withExecutable(t *testing.T, downloadErr error) (exe string, downloaded *[]string) {
	t.Helper()
	exe = filepath.Join(t.TempDir(), "aws-elevate")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.env.Executable = func() (string, error) { return exe, nil }
	h.env.TempDir = t.TempDir()
	var got []string
	h.env.Download = func(_ context.Context, v, goos, goarch string) ([]byte, error) {
		got = append(got, v+" "+goos+"/"+goarch)
		if downloadErr != nil {
			return nil, downloadErr
		}
		return []byte("client " + v), nil
	}
	return exe, &got
}

func content(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateInstallsTheNewestClientThatWorksWithTheInfrastructure(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.withReleases(releases) // 0.2.0 needs infrastructure 0.2.0; the harness has 0.0.1
	exe, downloaded := h.withExecutable(t, nil)

	if code := h.run("update", "--yes", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	want := "0.1.0 " + runtime.GOOS + "/" + runtime.GOARCH
	if len(*downloaded) != 1 || (*downloaded)[0] != want {
		t.Errorf("downloaded %v, want %s", *downloaded, want)
	}
	if got := content(t, exe); got != "client 0.1.0" {
		t.Errorf("executable %q", got)
	}
	if !strings.Contains(h.out.String(), "0.1.0") {
		t.Errorf("out %s", h.out)
	}
}

func TestUpdateWorksWhenTheServiceRefusesThisClient(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.withReleases(releases)
	h.svc.err = &api.Error{Status: 426, Code: api.CodeClientTooOld, Message: "client too old", InfraVersion: "0.2.0"}
	exe, _ := h.withExecutable(t, nil)

	if code := h.run("update", "--yes", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if got := content(t, exe); got != "client 0.2.0" {
		t.Errorf("executable %q", got)
	}
}

func TestUpdateWithNothingNewer(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.withReleases(releases[2:])
	exe, downloaded := h.withExecutable(t, nil)

	if code := h.run("update", "--yes", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, h.errOut)
	}
	if len(*downloaded) != 0 || content(t, exe) != "old" || !strings.Contains(h.out.String(), "newest") {
		t.Errorf("downloaded %v, out %s", *downloaded, h.out)
	}
}

func TestUpdateWithAWrongChecksumReplacesNothing(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.withReleases(releases)
	exe, _ := h.withExecutable(t, fmt.Errorf("%w: aws-elevate_linux_amd64 of release 0.1.0 is not the published file", release.ErrChecksum))

	if code := h.run("update", "--yes", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if content(t, exe) != "old" || !strings.Contains(h.errOut.String(), "checksum") {
		t.Errorf("stderr %s", h.errOut)
	}
}

func TestUpdateInAReadOnlyFolderSavesTheFileAndSaysWhatToRun(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	h := newHarness(t)
	h.setUp(t)
	h.withReleases(releases)
	exe, _ := h.withExecutable(t, nil)
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	if code := h.run("update", "--yes", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	saved := filepath.Join(h.env.TempDir, "aws-elevate-0.1.0")
	if content(t, exe) != "old" || content(t, saved) != "client 0.1.0" {
		t.Fatalf("exe %q", content(t, exe))
	}
	if want := "sudo install -m 0755 " + saved + " " + exe; !strings.Contains(h.errOut.String(), want) {
		t.Errorf("stderr %s\nwant %s", h.errOut, want)
	}
}

func TestUpdateCheckTurnedOff(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	calls := h.withReleases(releases)
	_, downloaded := h.withExecutable(t, nil)
	settings, _ := awsx.LoadSettings(h.env.SettingsPath)
	settings.CheckUpdates = ptr(false)
	if err := awsx.SaveSettings(h.env.SettingsPath, settings); err != nil {
		t.Fatal(err)
	}
	if code := h.run("update", "--yes", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if *calls != 0 || len(*downloaded) != 0 || !strings.Contains(h.errOut.String(), "checkUpdates") {
		t.Errorf("calls %d, stderr %s", *calls, h.errOut)
	}
}

func TestUpdateAsksBeforeReplacing(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.withReleases(releases)
	exe, downloaded := h.withExecutable(t, nil)
	h.typing("n\n")
	if code := h.run("update", "--profile", "work"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if len(*downloaded) != 0 || content(t, exe) != "old" {
		t.Errorf("replaced without confirmation: %v", *downloaded)
	}
	h = newHarness(t)
	h.setUp(t)
	h.withReleases(releases)
	exe, _ = h.withExecutable(t, nil)
	if code := h.run("update", "--profile", "work"); code != 1 || !strings.Contains(h.errOut.String(), "--yes") || content(t, exe) != "old" {
		t.Errorf("without a terminal: exit %d, %q", code, h.errOut)
	}
}

func TestUpdateWithYes(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.withReleases(releases)
	exe, _ := h.withExecutable(t, nil)
	if code := h.run("update", "--yes", "--profile", "work"); code != 0 || content(t, exe) != "client 0.1.0" {
		t.Errorf("exit %d: %s", code, h.errOut)
	}
}

func TestUpdatePrintsVersionsAndChecksum(t *testing.T) {
	h := newHarness(t)
	h.setUp(t)
	h.withReleases(releases)
	h.withExecutable(t, nil)
	if code := h.run("update", "--yes", "--profile", "work"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	sum := sha256.Sum256([]byte("client 0.1.0"))
	for _, want := range []string{version.Client + " → 0.1.0", "SHA-256 " + hex.EncodeToString(sum[:])} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("misses %q: %q", want, h.out)
		}
	}
}
