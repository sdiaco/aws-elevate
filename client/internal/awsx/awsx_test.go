package awsx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type fakePortal struct {
	accounts []PortalAccount
	roles    map[string][]string
	refusals int // Credentials fails this many times before succeeding
	calls    int
}

func (f *fakePortal) ListAccounts(context.Context) ([]PortalAccount, error) { return f.accounts, nil }

func (f *fakePortal) ListRoles(_ context.Context, account string) ([]string, error) {
	return f.roles[account], nil
}

func (f *fakePortal) Credentials(_ context.Context, account, role string) (aws.Credentials, error) {
	f.calls++
	if f.calls <= f.refusals {
		return aws.Credentials{}, errors.New("ForbiddenException: No access")
	}
	return aws.Credentials{AccessKeyID: account + "/" + role}, nil
}

type fakeCaller struct{ err error }

func (f fakeCaller) Check(context.Context, aws.Credentials) error { return f.err }

type fakeParams struct{ url string }

func (f fakeParams) APIURL(_ context.Context, creds aws.Credentials) (string, error) {
	return f.url + "#" + creds.AccessKeyID, nil
}

func newSession(p *fakePortal) (*Session, *[]time.Duration) {
	var slept []time.Duration
	s := &Session{
		Portal: p,
		Caller: fakeCaller{},
		Params: fakeParams{url: "https://api.example.com/v1"},
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}
	return s, &slept
}

func TestDiscoverOneCandidatePrefersAdmin(t *testing.T) {
	s, _ := newSession(&fakePortal{
		accounts: []PortalAccount{{ID: "111111111111", Name: "shared"}, {ID: "222222222222", Name: "workload"}},
		roles: map[string][]string{
			"111111111111": {"ReadOnly", "ElevateRequester", "ElevateAdmin"},
			"222222222222": {"ReadOnly"},
		},
	})
	got, err := s.Discover(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := Target{Account: "111111111111", Role: RoleAdmin, APIURL: "https://api.example.com/v1#111111111111/ElevateAdmin"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDiscoverTwoCandidatesAsksForAccount(t *testing.T) {
	p := &fakePortal{
		accounts: []PortalAccount{{ID: "111111111111", Name: "shared"}, {ID: "222222222222", Name: "other"}},
		roles: map[string][]string{
			"111111111111": {"ElevateRequester"},
			"222222222222": {"ElevateAdmin"},
		},
	}
	s, _ := newSession(p)
	_, err := s.Discover(context.Background(), "")
	if err == nil {
		t.Fatal("expected an error with two candidates")
	}
	for _, part := range []string{"111111111111 (shared)", "222222222222 (other)", "--account"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not mention %q", err, part)
		}
	}

	for _, pick := range []string{"222222222222", "other"} {
		got, err := s.Discover(context.Background(), pick)
		if err != nil {
			t.Fatal(err)
		}
		if got.Account != "222222222222" || got.Role != RoleAdmin {
			t.Errorf("--account %s: got %+v", pick, got)
		}
	}
}

func TestDiscoverNoCandidate(t *testing.T) {
	s, _ := newSession(&fakePortal{
		accounts: []PortalAccount{{ID: "111111111111", Name: "shared"}},
		roles:    map[string][]string{"111111111111": {"ReadOnly"}},
	})
	_, err := s.Discover(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "ElevateRequester") {
		t.Fatalf("got %v", err)
	}
	_, err = s.Discover(context.Background(), "999999999999")
	if err == nil {
		t.Fatal("expected an error for an account without a service role")
	}
}

func TestWaitReadyAfterThreeRefusals(t *testing.T) {
	p := &fakePortal{refusals: 3}
	s, slept := newSession(p)
	creds, err := s.WaitReady(context.Background(), "222222222222", "JIT-abcdefghij")
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessKeyID != "222222222222/JIT-abcdefghij" {
		t.Fatalf("got %+v", creds)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(*slept) != len(want) {
		t.Fatalf("slept %v, want %v", *slept, want)
	}
	for i := range want {
		if (*slept)[i] != want[i] {
			t.Fatalf("slept %v, want %v", *slept, want)
		}
	}
}

func TestWaitReadyRetriesWhenTheFirstCallIsDenied(t *testing.T) {
	p := &fakePortal{}
	s, slept := newSession(p)
	denials := 2
	s.Caller = callerFunc(func() error {
		if denials > 0 {
			denials--
			return errors.New("AccessDenied")
		}
		return nil
	})
	if _, err := s.WaitReady(context.Background(), "222222222222", "JIT-abcdefghij"); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 2 {
		t.Fatalf("slept %v", *slept)
	}
}

func TestWaitReadyGivesUpAfterSixtySeconds(t *testing.T) {
	p := &fakePortal{refusals: 1000}
	s, slept := newSession(p)
	_, err := s.WaitReady(context.Background(), "222222222222", "JIT-abcdefghij")
	if err == nil || !strings.Contains(err.Error(), "No access") {
		t.Fatalf("got %v", err)
	}
	var total time.Duration
	for _, d := range *slept {
		total += d
		if d > 8*time.Second {
			t.Fatalf("delay %v above 8s", d)
		}
	}
	if total < 52*time.Second || total > ReadyTimeout {
		t.Fatalf("waited %v in total", total)
	}
}

type callerFunc func() error

func (f callerFunc) Check(context.Context, aws.Credentials) error { return f() }

const otherProfiles = `[default]
region = eu-west-1

[profile work]
sso_session = corp
sso_account_id = 111111111111
sso_role_name = ReadOnly
region = eu-west-1

[sso-session corp]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-west-1
`

func TestWriteProfileKeepsOtherProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(otherProfiles), 0o600); err != nil {
		t.Fatal(err)
	}
	src := SourceProfile{Name: "work", SSOSession: "corp", SSORegion: "eu-west-1", Region: "eu-west-1"}

	name, err := WriteProfile(path, src, "AdministratorAccess", "222222222222", "JIT-abcdefghij")
	if err != nil {
		t.Fatal(err)
	}
	if name != "jit-AdministratorAccess-222222222222" {
		t.Fatalf("name %q", name)
	}
	// A second grant on the same source PS and account reuses the profile name.
	if _, err := WriteProfile(path, src, "AdministratorAccess", "222222222222", "JIT-klmnopqrst"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.HasPrefix(got, otherProfiles) {
		t.Fatalf("other profiles changed:\n%s", got)
	}
	want := "\n[profile jit-AdministratorAccess-222222222222]\n" +
		"sso_session = corp\n" +
		"sso_account_id = 222222222222\n" +
		"sso_role_name = JIT-klmnopqrst\n" +
		"region = eu-west-1\n"
	if got != otherProfiles+want {
		t.Fatalf("got:\n%s\nwant suffix:\n%s", got, want)
	}
}

func TestWriteProfileReplacesInTheMiddle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	before := "[profile jit-PS-222222222222]\nsso_role_name = JIT-old\n\n[profile after]\nregion = us-east-1\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	src := SourceProfile{Name: "legacy", SSOStartURL: "https://example.awsapps.com/start", SSORegion: "eu-west-1", Region: "eu-west-1"}
	if _, err := WriteProfile(path, src, "PS", "222222222222", "JIT-new"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := "[profile jit-PS-222222222222]\n" +
		"sso_start_url = https://example.awsapps.com/start\n" +
		"sso_region = eu-west-1\n" +
		"sso_account_id = 222222222222\n" +
		"sso_role_name = JIT-new\n" +
		"region = eu-west-1\n" +
		"\n[profile after]\nregion = us-east-1\n"
	if string(data) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", data, want)
	}
}

func TestWriteProfileCreatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config")
	src := SourceProfile{Name: "work", SSOSession: "corp", Region: "eu-west-1"}
	if _, err := WriteProfile(path, src, "PS", "222222222222", "JIT-abcdefghij"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(data), "[profile jit-PS-222222222222]\n") {
		t.Fatalf("got %q, %v", data, err)
	}
}

func TestConsoleLink(t *testing.T) {
	got := ConsoleLink("https://example.awsapps.com/start/", "222222222222", "JIT-abcdefghij")
	want := "https://example.awsapps.com/start/#/console?account_id=222222222222&role_name=JIT-abcdefghij"
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aws-elevate", "config.json")
	empty, err := LoadSettings(path)
	if err != nil || len(empty.Profiles) != 0 {
		t.Fatalf("missing file: %+v, %v", empty, err)
	}
	s := Settings{Profiles: map[string]Target{"work": {Account: "111111111111", Role: RoleRequester, APIURL: "https://api.example.com/v1"}}}
	if err := SaveSettings(path, s); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	got, err := LoadSettings(path)
	if err != nil || got.Profiles["work"] != s.Profiles["work"] {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestLoadSettingsRejectsBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(path, []byte("{"), 0o600)
	if _, err := LoadSettings(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("got %v", err)
	}
}
