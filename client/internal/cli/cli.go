// Package cli is the command line of aws-elevate. It parses arguments,
// calls the service and prints what comes back; every decision on grants and rules is
// taken by the service.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/release"
	"github.com/sdiaco/aws-elevate/client/internal/term"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// Env is everything a command touches outside the process, replaced in tests.
type Env struct {
	Stdout       io.Writer
	Stderr       io.Writer
	Stdin        io.Reader
	Getenv       func(string) string
	Environ      func() []string
	Open         func(ctx context.Context, profile string) (Backend, error)
	Profile      func(ctx context.Context, profile string) (awsx.SourceProfile, error)
	Installer    func(ctx context.Context, profile, region string) (Installer, error)
	Releases     func(ctx context.Context) ([]release.Release, error)
	Download     func(ctx context.Context, version, goos, goarch string) ([]byte, error)
	Executable   func() (string, error)
	TempDir      string
	Sleep        func(ctx context.Context, d time.Duration) error
	Shell        func(shell string, env []string) (int, error)
	OpenBrowser  func(url string) error
	SettingsPath string
	ConfigFile   string
	CacheDir     string // per-profile caches, ~/.cache/aws-elevate
	AliasFile    string
	LookPath     func(file string) (string, error)

	ui *term.UI // one per run: it buffers the input
}

// UI is the terminal of this run.
func (e *Env) UI() *term.UI {
	if e.ui == nil {
		in := e.Stdin
		if in == nil {
			in = strings.NewReader("")
		}
		e.ui = term.New(in, e.Stdout, e.Getenv)
	}
	return e.ui
}

// DefaultEnv wires the real process, AWS and files.
func DefaultEnv() (*Env, error) {
	settings, err := awsx.SettingsPath()
	if err != nil {
		return nil, err
	}
	alias, err := awsx.AliasFile()
	if err != nil {
		return nil, err
	}
	return &Env{
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
		Stdin:        os.Stdin,
		Getenv:       os.Getenv,
		Environ:      os.Environ,
		Open:         Open,
		Profile:      awsx.ReadProfile,
		Installer:    openInstaller,
		Releases:     release.New().List,
		Download:     release.New().Download,
		Executable:   executable,
		TempDir:      os.TempDir(),
		Sleep:        awsx.Sleep,
		Shell:        startShell,
		OpenBrowser:  openBrowser,
		SettingsPath: settings,
		ConfigFile:   awsx.ConfigFile(),
		AliasFile:    alias,
		CacheDir:     cacheDir(),
		LookPath:     exec.LookPath,
	}, nil
}

// cacheDir is ~/.cache/aws-elevate or its equivalent; empty when the system has none, and
// then nothing is cached.
func cacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "aws-elevate")
}

type command struct {
	summary string
	usage   string
	run     func(ctx context.Context, e *Env, args []string) error
}

// commands is filled in init: the commands themselves read it for their usage line.
var commands map[string]command

func init() {
	commands = map[string]command{
		"setup":   {"Find the service and add the `aws elevate` alias", "setup [--account <id|name>]", runSetup},
		"request": {"Request access; without arguments, list what you can request", "request [<rule> --account <id|name> [--duration 45m] --reason \"…\"]", runRequest},
		"shell":   {"Open a shell with an active grant's profile", "shell [<id>]", runShell},
		"status":  {"Your grants and requests, and requests you can approve", "status", runStatus},
		"revoke":  {"End one or more grants early, or cancel a pending request", "revoke [<id> …] --reason \"…\"", runRevoke},
		"approve": {"Approve a request, or reject it with --reject", "approve <id> [--reject --reason \"why\"]", runApprove},
		"ui":      {"Open the local web interface in your browser", "ui", runUI},
		"update":  {"Replace this executable with the newest version that works with your service", "update", runUpdate},
		"install": {"Install the service in the delegated administrator account", installUsage, runInstall},
		"admin":   {"Rules, export and import, installation checks (administrators)", adminUsage, runAdmin},
	}
}

// errUsage marks a wrong command line: the usage is printed after the message.
var errUsage = errors.New("usage")

// Run executes one command line and returns the process exit code.
func Run(ctx context.Context, e *Env, args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printHelp(e.Stdout)
		return 0
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Fprintf(e.Stdout, "aws-elevate %s\n", version.Client)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(e.Stderr, "aws-elevate: unknown command %q\n\n", args[0])
		printHelp(e.Stderr)
		return 2
	}
	err := cmd.run(ctx, e, args[1:])
	var code exitCode
	var expired *awsx.SessionExpiredError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &expired):
		fmt.Fprintf(e.Stderr, "aws-elevate: your AWS session for profile %s has expired. Run:\n  %s\n", expired.Profile, expired.Command())
		return 1
	case errors.Is(err, errUsage):
		fmt.Fprintf(e.Stderr, "aws-elevate: %v\nusage: aws elevate %s\n", strings.TrimSuffix(err.Error(), ": usage"), cmd.usage)
		return 2
	default:
		fmt.Fprintf(e.Stderr, "aws-elevate: %v\n", err)
		return 1
	}
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "aws-elevate: temporary elevated access on AWS IAM Identity Center")
	fmt.Fprintln(w, "\nusage: aws elevate <command> [--profile <p>] [options]")
	fmt.Fprintln(w, "\ncommands:")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-8s %s\n", n, commands[n].summary)
	}
	fmt.Fprintln(w, "\nEvery command takes --profile and --help. aws-elevate --version prints the version.")
}

// flags is a command's flag set with the common --profile.
type flags struct {
	*flag.FlagSet
	profile string
}

func newFlags(e *Env, name string) *flags {
	f := &flags{FlagSet: flag.NewFlagSet(name, flag.ContinueOnError)}
	f.SetOutput(e.Stderr)
	f.StringVar(&f.profile, "profile", "", "AWS CLI profile of your SSO session (default: $AWS_PROFILE, else default)")
	f.Usage = func() {
		fmt.Fprintf(e.Stderr, "usage: aws elevate %s\n", commands[name].usage)
		f.PrintDefaults()
	}
	return f
}

// parse accepts flags before and after positional arguments, as the AWS CLI does.
func (f *flags) parse(args []string) ([]string, error) {
	var positional []string
	for {
		if err := f.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, fmt.Errorf("%v: %w", err, errUsage)
		}
		args = f.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// profileName is --profile, else $AWS_PROFILE, else the profile of the last setup, else
// default. A settings file that cannot be read is reported by the command that loads it.
func (f *flags) profileName(e *Env) string {
	if f.profile == "" && e.Getenv("AWS_PROFILE") == "" {
		if s, err := awsx.LoadSettings(e.SettingsPath); err == nil && s.Default != "" {
			return s.Default
		}
	}
	return f.awsProfileName(e)
}

// awsProfileName follows the AWS CLI only: --profile, else $AWS_PROFILE, else default.
// Commands that act as an account administrator use it, never the profile set up for
// Elevate.
func (f *flags) awsProfileName(e *Env) string {
	if f.profile != "" {
		return f.profile
	}
	if p := e.Getenv("AWS_PROFILE"); p != "" {
		return p
	}
	return "default"
}

// setUpTarget is the service setup found for the profile.
func setUpTarget(e *Env, profile string) (awsx.Target, error) {
	settings, err := awsx.LoadSettings(e.SettingsPath)
	if err != nil {
		return awsx.Target{}, err
	}
	target, ok := settings.Profiles[profile]
	if !ok {
		hint := ""
		if names := slices.Sorted(maps.Keys(settings.Profiles)); len(names) > 0 {
			hint = " (set up: " + strings.Join(names, ", ") + ")"
		}
		return awsx.Target{}, fmt.Errorf("profile %q is not set up%s: run aws-elevate setup --profile %s", profile, hint, profile)
	}
	return target, nil
}

// connect opens the profile and the service found by setup.
func connect(ctx context.Context, e *Env, profile string) (Backend, Service, error) {
	target, err := setUpTarget(e, profile)
	if err != nil {
		return nil, nil, err
	}
	b, err := e.Open(ctx, profile)
	if err != nil {
		return nil, nil, err
	}
	svc, err := b.Service(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	return b, svc, nil
}

func startShell(shell string, env []string) (int, error) {
	cmd := exec.Command(shell)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, fmt.Errorf("start %s: %w", shell, err)
	}
	return 0, nil
}
