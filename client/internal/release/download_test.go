package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// downloads serves v0.2.0's checksums.txt and the executable for linux/arm64.
func downloads(t *testing.T, checksums string) *Lister {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0.2.0/checksums.txt":
			fmt.Fprint(w, checksums)
		case "/v0.2.0/aws-elevate_linux_arm64":
			fmt.Fprint(w, "new binary")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Lister{HTTP: srv.Client(), DownloadURL: srv.URL}
}

func TestDownloadVerifiesTheChecksum(t *testing.T) {
	good := sum([]byte("other")) + "  aws-elevate_darwin_arm64\n" + sum([]byte("new binary")) + "  aws-elevate_linux_arm64\n"
	got, err := downloads(t, good).Download(context.Background(), "0.2.0", "linux", "arm64")
	if err != nil || string(got) != "new binary" {
		t.Fatalf("%q %v", got, err)
	}
	bad := sum([]byte("tampered")) + "  aws-elevate_linux_arm64\n"
	if _, err := downloads(t, bad).Download(context.Background(), "0.2.0", "linux", "arm64"); !errors.Is(err, ErrChecksum) {
		t.Errorf("wrong checksum: %v", err)
	}
	missing := sum([]byte("new binary")) + "  aws-elevate_darwin_arm64\n"
	if _, err := downloads(t, missing).Download(context.Background(), "0.2.0", "linux", "arm64"); !errors.Is(err, ErrChecksum) {
		t.Errorf("no checksum line: %v", err)
	}
	if _, err := downloads(t, good).Download(context.Background(), "0.2.0", "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("no executable: %v", err)
	}
}

func TestReplaceSwapsTheExecutable(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "aws-elevate")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Replace(exe, []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	info, _ := os.Stat(exe)
	if string(got) != "new" || info.Mode().Perm() != 0o755 {
		t.Errorf("content %q mode %v", got, info.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".aws-elevate-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestReplaceInAReadOnlyFolderIsErrNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permissions are not enforced")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "aws-elevate")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := Replace(exe, []byte("new")); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("err %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Errorf("replaced anyway: %q", got)
	}
}
