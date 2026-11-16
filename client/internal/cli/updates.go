package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/release"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// runUpdates is `admin updates` (design §12.3): the new releases for this client and for
// the infrastructure, or with --preview the changes an infrastructure update would make
// and the command that applies it. The admin applies it with their own admin role:
// ElevateAdmin can only create and read previews.
func runUpdates(ctx context.Context, e *Env, args []string) error {
	f := newFlags(e, "admin")
	preview := f.String("preview", "", "infrastructure version to preview")
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q: %w", rest[0], errUsage)
	}
	profile := f.profileName(e)
	if *preview == "" {
		settings, err := awsx.LoadSettings(e.SettingsPath)
		if err != nil {
			return err
		}
		if !settings.UpdatesChecked() {
			return fmt.Errorf("the update check is turned off (checkUpdates: false in %s)", e.SettingsPath)
		}
	}
	s, err := connectAdmin(ctx, e, profile)
	if err != nil {
		return err
	}
	if *preview != "" {
		return showPreview(ctx, e, s, *preview)
	}
	rels, err := e.Releases(ctx)
	if err != nil {
		return err
	}
	return listUpdates(e.Stdout, rels, s.me.InfraVersion)
}

func listUpdates(out io.Writer, rels []release.Release, infra string) error {
	fmt.Fprintf(out, "Client %s, infrastructure %s.\n", version.Client, infra)
	clients := release.ClientUpdates(rels, version.Client, infra)
	infras := release.InfraUpdates(rels, infra)
	if len(clients) == 0 && len(infras) == 0 {
		fmt.Fprintln(out, "Everything is up to date.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if len(clients) > 0 {
		fmt.Fprintf(w, "\nClient updates that work with infrastructure %s:\n", infra)
		for _, r := range clients {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", r.Version, typeOf(r), r.Name, r.URL)
		}
		fmt.Fprintln(w, "  Install the newest with: aws-elevate update")
	}
	if len(infras) > 0 {
		fmt.Fprintln(w, "\nInfrastructure updates:")
		for _, r := range infras {
			note := ""
			if c, err := version.Cmp(version.Client, r.MinClient); err != nil || c < 0 {
				note = "needs client " + r.MinClient
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", r.Version, typeOf(r), r.Name, r.URL, note)
		}
		fmt.Fprintf(w, "  See what an update changes: aws elevate admin updates --preview %s\n", infras[0].Version)
	}
	return w.Flush()
}

func typeOf(r release.Release) string {
	if r.Type == "" {
		return "-"
	}
	return r.Type
}

func showPreview(ctx context.Context, e *Env, s *adminSession, v string) error {
	region, stackName, err := stackRegionAndName(s.inst.StackID)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://elevate-releases-%s.s3.%s.amazonaws.com/v%s/elevate.yaml", region, region, v)
	fmt.Fprintf(e.Stdout, "Previewing infrastructure %s on stack %s (nothing is changed)...\n", v, stackName)
	p, err := s.aws.PreviewUpdate(ctx, s.inst.StackID, url)
	if err != nil {
		return err
	}
	if p.NoChanges {
		fmt.Fprintln(e.Stdout, "No changes: the stack already matches this version.")
		return nil
	}
	w := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tRESOURCE\tTYPE\tREPLACEMENT")
	for _, c := range p.Changes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Action, c.Resource, c.Type, c.Replacement)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(e.Stdout, "\nTo apply it, run with your own admin role of the account (ElevateAdmin cannot):")
	cmd := []string{
		"aws cloudformation update-stack",
		"--profile <your admin profile> --region " + region,
		"--stack-name " + stackName,
		"--template-url " + url,
	}
	if len(p.Parameters) > 0 {
		params := make([]string, len(p.Parameters))
		for i, k := range p.Parameters {
			params[i] = "ParameterKey=" + k + ",UsePreviousValue=true"
		}
		cmd = append(cmd, "--parameters "+strings.Join(params, " "))
	}
	cmd = append(cmd, "--capabilities CAPABILITY_NAMED_IAM")
	fmt.Fprintln(e.Stdout, "  "+strings.Join(cmd, " \\\n    "))
	return nil
}

// stackRegionAndName reads arn:aws:cloudformation:<region>:<account>:stack/<name>/<id>.
func stackRegionAndName(arn string) (string, string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) == 6 {
		if res := strings.Split(parts[5], "/"); len(res) == 3 && res[0] == "stack" {
			return parts[3], res[1], nil
		}
	}
	return "", "", errors.New("unexpected stack id " + arn)
}
