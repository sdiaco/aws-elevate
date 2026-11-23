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

// ProfileName is stable from one grant to the next.
func ProfileName(sourcePs, account string) string {
	return "jit-" + sourcePs + "-" + account
}

// The grant profiles live in one block at the end of the user's CLI config, between two
// comment lines, so the rest of the file is never touched. The block stays
// last: in the INI format a comment does not end a section, so lines written after the
// block without a header would belong to its last profile.
const (
	blockBegin = "# >>> aws-elevate: managed block, rewritten automatically, do not edit >>>"
	blockEnd   = "# <<< aws-elevate <<<"
)

// WriteProfile adds or replaces the grant profile in the managed block of the CLI config
// file at path and leaves every line outside the block as it was.
func WriteProfile(path string, src SourceProfile, sourcePs, account, role string) (string, error) {
	name := ProfileName(sourcePs, account)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	outside, block, err := splitBlock(string(data))
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	header := "[profile " + name + "]"
	for _, l := range strings.Split(outside, "\n") {
		if strings.TrimSpace(l) == header {
			return "", fmt.Errorf("%s: profile %s already exists outside the aws-elevate block; rename or remove it", path, name)
		}
	}
	block = replaceSection(block, header, profileSection(name, src, account, role))
	if err := writeFileAtomic(path, []byte(joinBlock(outside, block))); err != nil {
		return "", err
	}
	return name, nil
}

// splitBlock returns the file without the managed block and the block's content.
func splitBlock(doc string) (outside, block string, err error) {
	lines := strings.SplitAfter(doc, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case blockBegin:
			if begin >= 0 {
				return "", "", errors.New("the aws-elevate block appears twice; remove one copy")
			}
			begin = i
		case blockEnd:
			if begin < 0 || end >= 0 {
				return "", "", errors.New("the aws-elevate block end line has no begin line; fix or remove the block")
			}
			end = i
		}
	}
	if begin < 0 {
		return doc, "", nil
	}
	if end < 0 {
		return "", "", errors.New("the aws-elevate block has no end line; fix or remove the block")
	}
	before := strings.Join(lines[:begin], "")
	after := strings.TrimLeft(strings.Join(lines[end+1:], ""), "\n")
	outside = before
	if after != "" {
		outside = strings.TrimRight(before, "\n")
		if outside != "" {
			outside += "\n\n"
		}
		outside += after
	}
	return outside, strings.Join(lines[begin+1:end], ""), nil
}

// joinBlock puts the managed block back at the end of the file, after a blank line.
func joinBlock(outside, block string) string {
	var b strings.Builder
	if strings.TrimSpace(outside) != "" {
		b.WriteString(strings.TrimRight(outside, "\n"))
		b.WriteString("\n\n")
	}
	b.WriteString(blockBegin + "\n")
	b.WriteString(strings.TrimRight(block, "\n") + "\n")
	b.WriteString(blockEnd + "\n")
	return b.String()
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

// replaceSection swaps the section that starts with header (up to the next section) for
// section, or appends section after a blank line.
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
		if strings.TrimSpace(doc) == "" {
			return section
		}
		return strings.TrimRight(doc, "\n") + "\n\n" + section
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

// ConsoleLink opens the console on the grant's account and role.
func ConsoleLink(startURL, account, role string) string {
	return strings.TrimSuffix(startURL, "/") + "/#/console?account_id=" + account + "&role_name=" + role
}

// RemoveManagedBlock takes the grant profiles out of the CLI config at path and keeps the
// rest of the file. It reports whether there was a block to remove.
func RemoveManagedBlock(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	doc := string(data)
	if !strings.Contains(doc, blockBegin) && !strings.Contains(doc, blockEnd) {
		return false, nil
	}
	outside, block, err := splitBlock(doc)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if outside == doc && block == "" {
		return false, nil // the markers appear only inside other lines
	}
	outside = strings.TrimRight(outside, "\n")
	if outside != "" {
		outside += "\n"
	}
	return true, writeFileAtomic(path, []byte(outside))
}

// GrantProfiles lists the profiles in the managed block of the CLI config at path.
func GrantProfiles(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	_, block, err := splitBlock(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var names []string
	for _, l := range strings.Split(block, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(l), "[profile "); ok {
			names = append(names, strings.TrimSuffix(name, "]"))
		}
	}
	return names, nil
}
