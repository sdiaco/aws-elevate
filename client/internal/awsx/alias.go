package awsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// AliasLine makes `aws elevate …` run this client (design §10.1).
const AliasLine = "elevate = !aws-elevate"

// AliasFile is the AWS CLI alias file, ~/.aws/cli/alias.
func AliasFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	return filepath.Join(home, ".aws", "cli", "alias"), nil
}

// WriteAlias adds the elevate alias under [toplevel] once. An existing elevate alias is
// left as it is; every other line of the file stays unchanged. It reports whether the
// file changed.
func WriteAlias(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	top := -1
	section := ""
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			section = t
			if t == "[toplevel]" {
				top = i
			}
			continue
		}
		if section == "[toplevel]" {
			if name, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(name) == "elevate" {
				return false, nil
			}
		}
	}

	var out string
	if top >= 0 {
		before := strings.Join(lines[:top+1], "")
		if !strings.HasSuffix(before, "\n") {
			before += "\n"
		}
		out = before + AliasLine + "\n" + strings.Join(lines[top+1:], "")
	} else {
		out = string(data)
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if out != "" {
			out += "\n"
		}
		out += "[toplevel]\n" + AliasLine + "\n"
	}
	if err := writeFileAtomic(path, []byte(out)); err != nil {
		return false, err
	}
	return true, nil
}
