package release

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ErrChecksum: the downloaded executable does not match checksums.txt of its release.
var ErrChecksum = errors.New("checksum mismatch")

// ErrNotWritable: the folder of the executable cannot be written by this user.
var ErrNotWritable = errors.New("folder not writable")

// AssetName is the executable of a release for one system and architecture; install.sh
// uses the same name.
func AssetName(goos, goarch string) string { return "aws-elevate_" + goos + "_" + goarch }

// Download fetches the executable of version for goos/goarch and checks it against the
// SHA-256 listed in the release's checksums.txt (sha256sum format).
func (l *Lister) Download(ctx context.Context, version, goos, goarch string) ([]byte, error) {
	name := AssetName(goos, goarch)
	sums, err := l.get(ctx, version, "checksums.txt")
	if err != nil {
		return nil, err
	}
	want, err := checksumOf(sums, name)
	if err != nil {
		return nil, err
	}
	exe, err := l.get(ctx, version, name)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(exe)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%w: %s of release %s is not the published file", ErrChecksum, name, version)
	}
	return exe, nil
}

func checksumOf(sums []byte, name string) (string, error) {
	lines := bufio.NewScanner(bytes.NewReader(sums))
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%w: checksums.txt lists no %s", ErrChecksum, name)
}

func (l *Lister) get(ctx context.Context, version, file string) ([]byte, error) {
	url := l.DownloadURL + "/v" + version + "/" + file
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	return b, nil
}

// Replace writes data next to exe and renames it over exe, so a failure never leaves a
// half-written executable.
func Replace(exe string, data []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".aws-elevate-*")
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %s", ErrNotWritable, dir)
	}
	if err != nil {
		return fmt.Errorf("write next to %s: %w", exe, err)
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write next to %s: %w", exe, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write next to %s: %w", exe, err)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return fmt.Errorf("write next to %s: %w", exe, err)
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	ok = true
	return nil
}
