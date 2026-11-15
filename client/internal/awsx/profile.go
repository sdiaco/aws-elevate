package awsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SourceProfile is the user's usual SSO profile, from which a grant profile copies how
// to sign in.
type SourceProfile struct {
	Name        string
	SSOSession  string // empty for a legacy profile with sso_start_url
	SSOStartURL string
	SSORegion   string
	Region      string
}

// ProfileName is stable from one grant to the next (design §10.3).
func ProfileName(sourcePs, account string) string {
	return "jit-" + sourcePs + "-" + account
}

// WriteProfile adds or replaces the grant profile in the CLI config file at path and
// leaves every other line of the file as it was.
func WriteProfile(path string, src SourceProfile, sourcePs, account, role string) (string, error) {
	name := ProfileName(sourcePs, account)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	section := profileSection(name, src, account, role)
	out := replaceSection(string(data), "[profile "+name+"]", section)
	if err := writeFileAtomic(path, []byte(out)); err != nil {
		return "", err
	}
	return name, nil
}

func profileSection(name string, src SourceProfile, account, role string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[profile %s]\n", name)
	if src.SSOSession != "" {
		fmt.Fprintf(&b, "sso_session = %s\n", src.SSOSession)
	} else {
		fmt.Fprintf(&b, "sso_start_url = %s\n", src.SSOStartURL)
		fmt.Fprintf(&b, "sso_region = %s\n", src.SSORegion)
	}
	fmt.Fprintf(&b, "sso_account_id = %s\n", account)
	fmt.Fprintf(&b, "sso_role_name = %s\n", role)
	if src.Region != "" {
		fmt.Fprintf(&b, "region = %s\n", src.Region)
	}
	return b.String()
}

// replaceSection swaps the section that starts with header (up to the blank lines before
// the next section) for section, or appends section after a blank line.
func replaceSection(doc, header, section string) string {
	lines := strings.SplitAfter(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == header {
			start = i
			break
		}
	}
	if start < 0 {
		if doc == "" {
			return section
		}
		if !strings.HasSuffix(doc, "\n") {
			doc += "\n"
		}
		return doc + "\n" + section
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}
	// Keep the blank lines that separate this section from the next one.
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" && end < len(lines) {
		end--
	}
	return strings.Join(lines[:start], "") + section + strings.Join(lines[end:], "")
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	mode := fs.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ConsoleLink opens the console on the grant's account and role (design §10.3, spike C12).
func ConsoleLink(startURL, account, role string) string {
	return strings.TrimSuffix(startURL, "/") + "/#/console?account_id=" + account + "&role_name=" + role
}
