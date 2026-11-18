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
	settings.Default = profile
	if err := awsx.SaveSettings(e.SettingsPath, settings); err != nil {
		return err
	}
	added, err := awsx.WriteAlias(e.AliasFile)
	if err != nil {
		return err
	}

	u := e.UI()
	u.OK(fmt.Sprintf("Signed in as %s (%s)", u.Bold(me.User.UserName), target.Role),
		fmt.Sprintf("account %s · infra %s", u.Bold(target.Account), me.InfraVersion))
	u.OK(fmt.Sprintf("Profile %s saved as default for aws-elevate", u.Bold(profile)), "")
	if added {
		u.OK("Alias added", e.AliasFile+": use aws elevate <command>")
	}
	if _, err := e.LookPath("aws-elevate"); err != nil {
		u.Warn(`aws-elevate is not on your PATH: the alias "aws elevate" cannot find it`,
			"move it to ~/.local/bin, or run install.sh")
	}
	// Only a requester can ask for access; everyone else starts from the interface.
	next := "aws elevate ui"
	if slices.Contains(me.Roles, "requester") {
		next = "aws elevate request"
	}
	// $AWS_PROFILE wins over the saved default: name the profile when it would.
	if env := e.Getenv("AWS_PROFILE"); env != "" && env != profile {
		next += " --profile " + profile
	}
	u.Next(next)
	return nil
}
