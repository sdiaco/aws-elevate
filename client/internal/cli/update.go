package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/release"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// runUpdate replaces this executable with the newest release whose client works with the
// connected infrastructure, after checking its SHA-256.
func runUpdate(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "update")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	settings, err := awsx.LoadSettings(e.SettingsPath)
	if err != nil {
		return err
	}
	if !settings.UpdatesChecked() {
		return fmt.Errorf("the update check is turned off (checkUpdates: false in %s)", e.SettingsPath)
	}
	infra, err := infraVersion(ctx, e, f.profileName(e))
	if err != nil {
		return err
	}
	rels, err := e.Releases(ctx)
	if err != nil {
		return err
	}
	candidates := release.ClientUpdates(rels, version.Client, infra)
	if len(candidates) == 0 {
		fmt.Fprintf(e.Stdout, "aws-elevate %s is the newest version that works with infrastructure %s.\n", version.Client, infra)
		return nil
	}
	fmt.Fprintf(e.Stdout, "Downloading aws-elevate %s...\n", candidates[0].Version)
	msg, err := installClient(ctx, e, candidates[0].Version)
	if err != nil {
		return err
	}
	fmt.Fprintln(e.Stdout, msg)
	return nil
}

// installClient downloads client release v, checks it and replaces this executable. The
// interface uses it too.
func installClient(ctx context.Context, e *Env, v string) (string, error) {
	exe, err := e.Executable()
	if err != nil {
		return "", fmt.Errorf("find this executable: %w", err)
	}
	data, err := e.Download(ctx, v, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	err = release.Replace(exe, data)
	if errors.Is(err, release.ErrNotWritable) {
		return "", savedForManualInstall(e, v, exe, data, err)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Updated %s from %s to %s.", exe, version.Client, v), nil
}

// infraVersion asks the service, which tells its version even when it refuses this client.
func infraVersion(ctx context.Context, e *Env, profile string) (string, error) {
	_, svc, err := connect(ctx, e, profile)
	if err != nil {
		return "", err
	}
	me, err := svc.Me(ctx)
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Code == api.CodeClientTooOld && apiErr.InfraVersion != "" {
		return apiErr.InfraVersion, nil
	}
	if err != nil {
		return "", err
	}
	return me.InfraVersion, nil
}

// savedForManualInstall keeps the checked executable in the temporary folder and says how
// to install it with the rights the folder needs.
func savedForManualInstall(e *Env, v, exe string, data []byte, cause error) error {
	saved := filepath.Join(e.TempDir, "aws-elevate-"+v)
	if err := os.WriteFile(saved, data, 0o755); err != nil {
		return fmt.Errorf("%v; saving the download to %s failed too: %w", cause, saved, err)
	}
	return fmt.Errorf("%v: the checked aws-elevate %s is in %s; install it with:\n  sudo install -m 0755 %s %s", cause, v, saved, saved, exe)
}

// executable is this program's file, symbolic links resolved: the file to replace.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
