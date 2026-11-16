package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installScript runs install.sh against a release folder served as file:// URLs; every
// executable holds its own asset name and sums gives checksums.txt for a content.
func installScript(t *testing.T, sums func(content string) string) (prefix string, out string, err error) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	releases := t.TempDir()
	dir := filepath.Join(releases, "download", "v0.2.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, goos := range []string{"linux", "darwin"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			name := AssetName(goos, goarch)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, sums(name)+"  "+name)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prefix = filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command("sh", "../../../install.sh")
	cmd.Env = append(os.Environ(), "AWS_ELEVATE_RELEASES=file://"+releases, "VERSION=0.2.0", "PREFIX="+prefix)
	b, err := cmd.CombinedOutput()
	return prefix, string(b), err
}

func TestInstallScriptChecksAndInstalls(t *testing.T) {
	prefix, out, err := installScript(t, func(c string) string { return sum([]byte(c)) })
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	exe := filepath.Join(prefix, "aws-elevate")
	got, err := os.ReadFile(exe)
	if err != nil || !strings.HasPrefix(string(got), "aws-elevate_") {
		t.Fatalf("installed %q %v\n%s", got, err, out)
	}
	if info, _ := os.Stat(exe); info.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", info.Mode())
	}
}

func TestInstallScriptRefusesAWrongChecksum(t *testing.T) {
	prefix, out, err := installScript(t, func(string) string { return sum([]byte("tampered")) })
	if err == nil || !strings.Contains(out, "checksum") {
		t.Fatalf("err %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "aws-elevate")); !os.IsNotExist(err) {
		t.Errorf("installed anyway: %v", err)
	}
}
