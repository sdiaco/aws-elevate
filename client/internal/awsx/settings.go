package awsx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Settings is what setup found, per source profile, so later commands skip discovery,
// and the user's choices.
type Settings struct {
	Profiles map[string]Target `json:"profiles"`
	// Default is the profile of the last setup, used when neither --profile nor
	// $AWS_PROFILE names one.
	Default string `json:"default,omitempty"`
	// CheckUpdates false stops every call to the public release list.
	CheckUpdates *bool `json:"checkUpdates,omitempty"`
}

// UpdatesChecked is true unless the user turned the check off.
func (s Settings) UpdatesChecked() bool { return s.CheckUpdates == nil || *s.CheckUpdates }

// SettingsPath is ~/.config/aws-elevate/config.json, or under $XDG_CONFIG_HOME when set.
func SettingsPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "aws-elevate", "config.json"), nil
}

// LoadSettings returns empty settings when the file does not exist yet.
func LoadSettings(path string) (Settings, error) {
	s := Settings{Profiles: map[string]Target{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("read %s: %w (fix or delete the file, then run aws-elevate setup)", path, err)
	}
	if s.Profiles == nil {
		s.Profiles = map[string]Target{}
	}
	return s, nil
}

func SaveSettings(path string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// RemoveSettings deletes the settings file, and its folder once empty. It reports whether
// there was a file to delete.
func RemoveSettings(path string) (bool, error) {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("delete %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		if err := os.Remove(dir); err != nil {
			return true, fmt.Errorf("delete %s: %w", dir, err)
		}
	}
	return true, nil
}
