package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

// clientLeftovers is what aws-elevate left on this computer; an empty field is not there.
type clientLeftovers struct {
	block    bool     // the aws-elevate block is in the AWS CLI config, even empty
	profiles []string // grant profiles in the block
	settings string
	cache    string
	alias    string
	foreign  bool // the alias file has an elevate alias that is not ours
	exe      string
}

func (l clientLeftovers) empty() bool {
	return !l.block && len(l.profiles) == 0 && l.settings == "" && l.cache == "" && l.alias == "" && l.exe == ""
}

// findClientLeftovers looks at the places aws-elevate writes to and keeps what exists.
func findClientLeftovers(e *Env) (clientLeftovers, error) {
	var l clientLeftovers
	var err error
	if l.block, err = awsx.HasManagedBlock(e.ConfigFile); err != nil {
		return l, err
	}
	if l.profiles, err = awsx.GrantProfiles(e.ConfigFile); err != nil {
		return l, err
	}
	if exists(e.SettingsPath) {
		l.settings = e.SettingsPath
	}
	if e.CacheDir != "" && exists(e.CacheDir) {
		l.cache = e.CacheDir
	}
	ours, foreign, err := awsx.FindAlias(e.AliasFile)
	if err != nil {
		return l, err
	}
	if ours {
		l.alias = e.AliasFile
	}
	l.foreign = foreign
	exe, err := e.Executable()
	if err != nil {
		return l, fmt.Errorf("find this executable: %w", err)
	}
	if exists(exe) {
		l.exe = exe
	}
	return l, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// runUninstallClient removes what aws-elevate left on this computer, the executable last.
// It never calls AWS.
func runUninstallClient(_ context.Context, e *Env, args []string) error {
	f := newFlags(e, "uninstall client")
	check := f.Bool("check", false, "list what is on this computer and change nothing")
	yes := f.Bool("yes", false, "confirm without asking")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	u := e.UI()
	left, err := findClientLeftovers(e)
	if err != nil {
		return err
	}
	if left.foreign {
		u.Warn(fmt.Sprintf("The elevate alias in %s was not set by aws-elevate: it stays as it is", e.AliasFile))
	}
	if left.empty() {
		u.OK("Nothing to remove: aws-elevate is not on this computer", "")
		return nil
	}
	printClientLeftovers(e, left)
	if *check {
		return nil
	}
	if len(left.profiles) > 0 {
		u.Warn("Grant profiles found: " + strings.Join(left.profiles, ", ") +
			". Grants may still be open: revoke them first with aws elevate revoke")
	}
	ok, err := u.Confirm("Remove aws-elevate from this computer?", *yes)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("uninstall stopped: nothing was removed")
	}
	answered(u, *yes)
	return removeClient(e, left)
}

func printClientLeftovers(e *Env, l clientLeftovers) {
	w := e.UI().Writer()
	item := func(path, what string) { fmt.Fprintf(w, "  • %s   %s\n", path, e.UI().Dim(what)) }
	fmt.Fprintln(w, "  On this computer:")
	if l.block {
		item(e.ConfigFile, "the aws-elevate block")
	}
	if l.settings != "" {
		item(l.settings, "settings")
	}
	if l.cache != "" {
		item(l.cache, "cache")
	}
	if l.alias != "" {
		item(l.alias, "the elevate alias line")
	}
	if l.exe != "" {
		item(l.exe, "this program")
	}
	fmt.Fprintln(w)
}

// removeClient takes the files away. The executable goes last: on Linux and macOS the
// running process goes on after its file is deleted, and if a step fails the command is
// still there to run again.
func removeClient(e *Env, l clientLeftovers) error {
	u := e.UI()
	if l.block {
		if _, err := awsx.RemoveManagedBlock(e.ConfigFile); err != nil {
			return err
		}
		u.OK("Grant profiles removed", e.ConfigFile)
	}
	if l.settings != "" {
		if _, err := awsx.RemoveSettings(e.SettingsPath); err != nil {
			return err
		}
		u.OK("Settings removed", l.settings)
	}
	if l.cache != "" {
		if err := removeCache(l.cache); err != nil {
			return err
		}
		u.OK("Cache removed", l.cache)
	}
	if l.alias != "" {
		if _, _, err := awsx.RemoveAlias(e.AliasFile); err != nil {
			return err
		}
		u.OK("Alias removed", l.alias)
	}
	if l.exe != "" {
		if err := os.Remove(l.exe); err != nil {
			return fmt.Errorf("delete %s: %w (delete it by hand)", l.exe, err)
		}
		u.OK("Program removed", l.exe)
	}
	u.OK("aws-elevate is removed from this computer", "")
	return nil
}

// removeCache deletes the cache folder; it refuses any folder that is not named for this
// program, so a wrong setting can never reach another folder.
func removeCache(dir string) error {
	if filepath.Base(dir) != "aws-elevate" {
		return fmt.Errorf("not deleting %s: it is not an aws-elevate cache folder", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete %s: %w", dir, err)
	}
	return nil
}
