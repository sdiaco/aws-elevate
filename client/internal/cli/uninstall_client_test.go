package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

const clientConfigBefore = "[profile work]\nregion = eu-west-1\n"

const clientBlock = "\n# >>> aws-elevate: managed block, rewritten automatically, do not edit >>>\n" +
	"[profile jit-ssm-222222222222]\nregion = eu-west-1\n" +
	"[profile jit-ssm-333333333333]\nregion = eu-west-1\n" +
	"# <<< aws-elevate <<<\n"

// clientFiles is everything aws-elevate leaves on this computer, in a temporary folder.
type clientFiles struct {
	exe, cacheFile string
}

// withClientFiles installs a fake client on the harness: a grant block, settings, a
// cache, the alias and the executable.
func (h *harness) withClientFiles(t *testing.T) clientFiles {
	t.Helper()
	h.setUp(t)
	dir := t.TempDir()
	h.env.CacheDir = filepath.Join(dir, "cache", "aws-elevate")
	f := clientFiles{exe: filepath.Join(dir, "bin", "aws-elevate"), cacheFile: filepath.Join(h.env.CacheDir, "work.json")}
	for path, content := range map[string]string{
		h.env.ConfigFile: clientConfigBefore + clientBlock,
		h.env.AliasFile:  "[toplevel]\nelevate = !aws-elevate\nwhoami = sts get-caller-identity\n",
		f.cacheFile:      "{}",
		f.exe:            "binary",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.env.Executable = func() (string, error) { return f.exe, nil }
	return f
}

func TestUninstallClientListsWhatGoesAndAsks(t *testing.T) {
	h := newHarness(t)
	f := h.withClientFiles(t)
	h.typing("n\n")

	if code := h.run("uninstall", "client"); code == 0 {
		t.Fatalf("exit 0 after no: %s", h.out)
	}
	out := h.out.String()
	for _, want := range []string{h.env.ConfigFile, h.env.SettingsPath, h.env.CacheDir, h.env.AliasFile, f.exe, "Remove aws-elevate from this computer? [y/N]"} {
		if !strings.Contains(out, want) {
			t.Errorf("misses %q:\n%s", want, out)
		}
	}
	for _, path := range []string{h.env.ConfigFile, h.env.SettingsPath, f.cacheFile, h.env.AliasFile, f.exe} {
		if !exists(path) {
			t.Errorf("%s was removed", path)
		}
	}
}

func TestUninstallClientWarnsAboutGrantProfiles(t *testing.T) {
	h := newHarness(t)
	h.withClientFiles(t)
	h.typing("n\n")

	h.run("uninstall", "client")
	out := h.out.String()
	warn := "Grant profiles found: jit-ssm-222222222222, jit-ssm-333333333333. Grants may still be open: revoke them first with aws elevate revoke"
	at, ask := strings.Index(out, warn), strings.Index(out, "Remove aws-elevate from this computer?")
	if at < 0 || at > ask {
		t.Errorf("the warning is missing or after the question:\n%s", out)
	}
}

func TestUninstallClientRemovesEverything(t *testing.T) {
	h := newHarness(t)
	f := h.withClientFiles(t)

	if code := h.run("uninstall", "client", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if got, _ := os.ReadFile(h.env.ConfigFile); string(got) != clientConfigBefore {
		t.Errorf("config %q", got)
	}
	if got, _ := os.ReadFile(h.env.AliasFile); string(got) != "[toplevel]\nwhoami = sts get-caller-identity\n" {
		t.Errorf("alias %q", got)
	}
	for _, path := range []string{h.env.SettingsPath, h.env.CacheDir, f.exe} {
		if exists(path) {
			t.Errorf("%s is still there", path)
		}
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "aws-elevate is removed from this computer") {
		t.Errorf("last line %q", last)
	}
}

func TestUninstallClientCheckChangesNothing(t *testing.T) {
	h := newHarness(t)
	f := h.withClientFiles(t)

	if code := h.run("uninstall", "client", "--check"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	out := h.out.String()
	for _, want := range []string{h.env.ConfigFile, h.env.SettingsPath, h.env.CacheDir, h.env.AliasFile, f.exe} {
		if !strings.Contains(out, want) {
			t.Errorf("misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Remove aws-elevate") {
		t.Errorf("asked a question:\n%s", out)
	}
	for _, path := range []string{h.env.ConfigFile, h.env.SettingsPath, f.cacheFile, h.env.AliasFile, f.exe} {
		if !exists(path) {
			t.Errorf("%s was removed", path)
		}
	}
}

func TestUninstallClientTouchesNoAWS(t *testing.T) {
	h := newHarness(t)
	h.withClientFiles(t)
	h.env.Open = func(context.Context, string) (Backend, error) {
		t.Error("Open called")
		return nil, errors.New("no AWS")
	}
	h.env.Uninstaller = func(context.Context, string, string, string) (Uninstaller, error) {
		t.Error("Uninstaller called")
		return nil, errors.New("no AWS")
	}
	h.env.Profile = func(context.Context, string) (awsx.SourceProfile, error) {
		t.Error("Profile called")
		return awsx.SourceProfile{}, errors.New("no AWS")
	}
	if code := h.run("uninstall", "client", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
}

func TestUninstallClientWithNothingToRemove(t *testing.T) {
	h := newHarness(t)
	h.env.Executable = func() (string, error) { return filepath.Join(t.TempDir(), "gone"), nil }
	if code := h.run("uninstall", "client", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !strings.Contains(h.out.String(), "Nothing to remove") {
		t.Errorf("output:\n%s", h.out)
	}
}

func TestUninstallClientKeepsAForeignAliasAndSaysSo(t *testing.T) {
	h := newHarness(t)
	h.withClientFiles(t)
	foreign := "[toplevel]\nelevate = !my-own\n"
	if err := os.WriteFile(h.env.AliasFile, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run("uninstall", "client", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if got, _ := os.ReadFile(h.env.AliasFile); string(got) != foreign {
		t.Errorf("alias %q", got)
	}
	if !strings.Contains(h.out.String(), "not set by aws-elevate") {
		t.Errorf("no warning:\n%s", h.out)
	}
}

func TestUninstallClientWithoutTerminalNeedsYes(t *testing.T) {
	h := newHarness(t)
	f := h.withClientFiles(t)
	if code := h.run("uninstall", "client"); code == 0 || !strings.Contains(h.errOut.String(), "--yes") {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !exists(f.exe) {
		t.Error("the executable was removed")
	}
}
