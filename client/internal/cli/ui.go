package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/ui"
)

// uiIdle closes the interface left unused (design §8.6).
var uiIdle = 30 * time.Minute

func runUI(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "ui")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	profile := f.profileName(e)
	target, err := setUpTarget(e, profile)
	if err != nil {
		return err
	}
	b, svc, err := connect(ctx, e, profile)
	if err != nil {
		return err
	}
	me, err := svc.Me(ctx)
	if err != nil {
		return err
	}
	var admin ui.Admin
	if me.Installation != nil { // only ElevateAdmin gets it (design §7.2)
		aws, err := b.Admin(ctx, target)
		if err != nil {
			return err
		}
		admin = newUIAdmin(e, profile, aws, me.Installation)
	}
	updates, err := uiUpdates(e)
	if err != nil {
		return err
	}
	app := &ui.App{
		Me:      *me,
		Service: svc,
		Ready: func(ctx context.Context, g api.Grant) (string, error) {
			role := RoleName(g.ID)
			if err := b.WaitReady(ctx, g.Account, role); err != nil {
				return "", err
			}
			return awsx.WriteProfile(e.ConfigFile, b.Source(), g.SourcePs.Name, g.Account, role)
		},
		Console: func(g api.Grant) string {
			return awsx.ConsoleLink(b.Source().SSOStartURL, g.Account, RoleName(g.ID))
		},
		Admin:   admin,
		Updates: updates,
		Reopen:  "aws elevate ui --profile " + profile,
	}
	ln, err := ui.Listen()
	if err != nil {
		return err
	}
	defer ln.Close()
	s, err := ui.New(ln, ui.Pages(app), uiIdle)
	if err != nil {
		return err
	}
	url := s.StartURL()
	if err := e.OpenBrowser(url); err != nil {
		fmt.Fprintf(e.Stdout, "Open this address in your browser (it works once): %s\n", url)
	} else {
		fmt.Fprintf(e.Stdout, "Opened %s in your browser.\n", url)
	}
	fmt.Fprintln(e.Stdout, "The interface stops with Ctrl+C or after 30 minutes without use.")
	err = s.Serve(ctx)
	if errors.Is(err, ui.ErrIdle) {
		fmt.Fprintf(e.Stdout, "Closed after 30 minutes without use. Open it again with:\n  %s\n", app.Reopen)
		return nil
	}
	return err
}

// uiUpdates gives the Updates view the same releases and client update as the commands.
func uiUpdates(e *Env) (*ui.Updates, error) {
	settings, err := awsx.LoadSettings(e.SettingsPath)
	if err != nil {
		return nil, err
	}
	return &ui.Updates{
		Checked:      settings.UpdatesChecked(),
		SettingsPath: e.SettingsPath,
		Releases:     e.Releases,
		InstallClient: func(ctx context.Context, v string) (string, error) {
			return installClient(ctx, e, v)
		},
	}, nil
}

// openBrowser asks the desktop to open url and does not wait for the browser.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open the browser: %w", err)
	}
	// Reap the opener; its exit status says nothing about the page.
	go cmd.Wait()
	return nil
}
