package cli

import (
	"context"
	"fmt"
	"slices"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

// runSetup finds the service through the SSO session, remembers it for the
// profile and adds the `aws elevate` alias.
func runSetup(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "setup")
	account := f.String("account", "", "service account, id or name, when more than one is found")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	profile := f.profileName(e)

	b, err := e.Open(ctx, profile)
	if err != nil {
		return err
	}
	target, err := b.Discover(ctx, *account)
	if err != nil {
		return err
	}
	svc, err := b.Service(ctx, target)
	if err != nil {
		return err
	}
	me, err := svc.Me(ctx)
	if err != nil {
		return err
	}

	settings, err := awsx.LoadSettings(e.SettingsPath)
	if err != nil {
		return err
	}
	settings.Profiles[profile] = target
	if err := awsx.SaveSettings(e.SettingsPath, settings); err != nil {
		return err
	}
	added, err := awsx.WriteAlias(e.AliasFile)
	if err != nil {
		return err
	}

	fmt.Fprintf(e.Stdout, "Signed in as %s (%s) in account %s, infrastructure %s.\n",
		me.User.UserName, target.Role, target.Account, me.InfraVersion)
	if added {
		fmt.Fprintf(e.Stdout, "Added the alias to %s: use aws elevate <command>.\n", e.AliasFile)
	}
	// Only a requester can ask for access; everyone else starts from the interface.
	next := "aws elevate ui"
	if slices.Contains(me.Roles, "requester") {
		next = "aws elevate request"
	}
	fmt.Fprintln(e.Stdout, "Next: "+next)
	return nil
}
