package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/term"
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
	if f.profile == "" && e.Getenv("AWS_PROFILE") == "" {
		if profile, err = ssoProfileOrChoice(ctx, e, profile); err != nil {
			return err
		}
	}

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
			"move it to ~/.local/bin, or install it again with: curl -fsSL https://raw.githubusercontent.com/sdiaco/aws-elevate/main/install.sh | sh")
	}
	next := "aws elevate ui"
	// $AWS_PROFILE wins over the saved default: name the profile when it would.
	if env := e.Getenv("AWS_PROFILE"); env != "" && env != profile {
		next += " --profile " + profile
	}
	u.Next(next)
	return nil
}

// ssoProfileOrChoice keeps profile when it signs in through IAM Identity Center. When it
// does not (typically a missing default profile), the user picks one of the SSO profiles
// of the CLI config.
func ssoProfileOrChoice(ctx context.Context, e *Env, profile string) (string, error) {
	if src, err := e.Profile(ctx, profile); err == nil && src.SSOStartURL != "" {
		return profile, nil
	}
	names, err := awsx.SSOProfiles(e.ConfigFile)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no SSO profile in %s: create one with aws configure sso, then run aws-elevate setup --profile <name>", e.ConfigFile)
	}
	u := e.UI()
	if !u.Interactive() {
		return "", fmt.Errorf("profile %q is not an SSO profile: run aws-elevate setup --profile <name> (SSO profiles found: %s)", profile, strings.Join(names, ", "))
	}
	items := make([]term.Item, len(names))
	for i, n := range names {
		items[i] = term.Item{Title: n}
	}
	i, err := u.Select("SSO profile to use", items, false)
	if err != nil {
		return "", err
	}
	return names[i], nil
}
