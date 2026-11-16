package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/ui"
)

// uiIdle closes the interface left unused (design §8.6).
const uiIdle = 30 * time.Minute

func runUI(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "ui")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	b, svc, err := connect(ctx, e, f.profileName(e))
	if err != nil {
		return err
	}
	me, err := svc.Me(ctx)
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
	return s.Serve(ctx)
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
