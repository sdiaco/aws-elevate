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
	lasts    time.Duration // when set, the credentials expire this long after they are issued
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
	c := aws.Credentials{AccessKeyID: account + "/" + role, SecretAccessKey: "secret"}
	if f.lasts != 0 {
		c.CanExpire, c.Expires = true, time.Now().Add(f.lasts)
	}
	return c, nil
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

func TestDiscoverPrefersRequesterOverAuditorAndAcceptsAuditorAlone(t *testing.T) {
	for _, tc := range []struct {
		roles []string
		want  string
	}{
		{[]string{"ElevateAuditor", "ElevateRequester"}, RoleRequester},
		{[]string{"ElevateAuditor"}, RoleAuditor},
	} {
		s, _ := newSession(&fakePortal{
			accounts: []PortalAccount{{ID: "111111111111", Name: "shared"}},
			roles:    map[string][]string{"111111111111": tc.roles},
		})
		got, err := s.Discover(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Role != tc.want {
			t.Errorf("roles %v: got %s, want %s", tc.roles, got.Role, tc.want)
		}
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

const wantBegin = "# >>> aws-elevate: managed block, rewritten automatically, do not edit >>>\n"
const wantEnd = "# <<< aws-elevate <<<\n"

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var corp = SourceProfile{Name: "work", SSOSession: "corp", SSORegion: "eu-west-1", Region: "eu-west-1"}

func TestWriteProfileAppendsTheManagedBlock(t *testing.T) {
	path := writeConfig(t, otherProfiles)

	name, err := WriteProfile(path, corp, "AdministratorAccess", "222222222222", "JIT-abcdefghij")
	if err != nil {
		t.Fatal(err)
	}
	if name != "jit-AdministratorAccess-222222222222" {
		t.Fatalf("name %q", name)
	}
	want := otherProfiles + "\n" + wantBegin +
		"[profile jit-AdministratorAccess-222222222222]\n" +
		"sso_session = corp\n" +
		"sso_account_id = 222222222222\n" +
		"sso_role_name = JIT-abcdefghij\n" +
		"region = eu-west-1\n" +
		wantEnd
	if got := readConfig(t, path); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteProfileReplacesAndAddsInsideTheBlock(t *testing.T) {
	path := writeConfig(t, otherProfiles)
	for _, w := range []struct{ ps, role string }{{"A", "JIT-old"}, {"B", "JIT-b"}, {"A", "JIT-new"}} {
		if _, err := WriteProfile(path, corp, w.ps, "222222222222", w.role); err != nil {
			t.Fatal(err)
		}
	}
	got := readConfig(t, path)
	if !strings.HasPrefix(got, otherProfiles) {
		t.Fatalf("user profiles changed:\n%s", got)
	}
	if strings.Count(got, wantBegin) != 1 || strings.Count(got, wantEnd) != 1 || !strings.HasSuffix(got, wantEnd) {
		t.Fatalf("one block at the end expected:\n%s", got)
	}
	if strings.Contains(got, "JIT-old") || !strings.Contains(got, "JIT-new") || !strings.Contains(got, "[profile jit-B-222222222222]") {
		t.Fatalf("profiles not replaced or kept:\n%s", got)
	}
}

func TestWriteProfileMovesTheBlockToTheEnd(t *testing.T) {
	// Lines written after the block would belong to its last section, so the block
	// always goes back to the end; the user's lines keep their order.
	user := "[profile after]\nregion = us-east-1\n"
	path := writeConfig(t, wantBegin+"[profile jit-A-222222222222]\nsso_role_name = JIT-old\n"+wantEnd+"\n"+user)
	if _, err := WriteProfile(path, corp, "B", "222222222222", "JIT-b"); err != nil {
		t.Fatal(err)
	}
	got := readConfig(t, path)
	if !strings.HasPrefix(got, user) || !strings.HasSuffix(got, wantEnd) {
		t.Fatalf("got:\n%s", got)
	}
	if !strings.Contains(got, "[profile jit-A-222222222222]") {
		t.Fatalf("existing managed profile lost:\n%s", got)
	}
}

func TestWriteProfileRefusesAProfileOutsideTheBlock(t *testing.T) {
	before := "[profile jit-PS-222222222222]\nsso_role_name = Mine\n"
	path := writeConfig(t, before)
	_, err := WriteProfile(path, corp, "PS", "222222222222", "JIT-new")
	if err == nil || !strings.Contains(err.Error(), "jit-PS-222222222222") {
		t.Fatalf("want an error naming the profile, got %v", err)
	}
	if readConfig(t, path) != before {
		t.Fatal("file changed")
	}
}

func TestWriteProfileRefusesABrokenBlock(t *testing.T) {
	before := otherProfiles + wantBegin + "[profile jit-A-222222222222]\n"
	path := writeConfig(t, before)
	if _, err := WriteProfile(path, corp, "B", "222222222222", "JIT-b"); err == nil {
		t.Fatal("want an error for a block without its end line")
	}
	if readConfig(t, path) != before {
		t.Fatal("file changed")
	}
}

func TestWriteProfileLegacySourceAndNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config")
	src := SourceProfile{Name: "legacy", SSOStartURL: "https://example.awsapps.com/start", SSORegion: "eu-west-1"}
	if _, err := WriteProfile(path, src, "PS", "222222222222", "JIT-abcdefghij"); err != nil {
		t.Fatal(err)
	}
	want := wantBegin + "[profile jit-PS-222222222222]\n" +
		"sso_start_url = https://example.awsapps.com/start\n" +
		"sso_region = eu-west-1\n" +
		"sso_account_id = 222222222222\n" +
		"sso_role_name = JIT-abcdefghij\n" + wantEnd
	if got := readConfig(t, path); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
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

func TestRoleCredentialsAreFetchedAgainBeforeTheyExpire(t *testing.T) {
	// Seen in the UI pass: credentials read once at start expired after an hour and every
	// page answered "security token expired" until the interface was restarted.
	ctx := context.Background()
	fresh := &fakePortal{lasts: time.Hour}
	p := RoleCredentials(fresh, "111111111111", "ElevateAdmin")
	for range 3 {
		if c, err := p.Retrieve(ctx); err != nil || c.AccessKeyID != "111111111111/ElevateAdmin" {
			t.Fatalf("%v %v", c, err)
		}
	}
	if fresh.calls != 1 {
		t.Errorf("valid credentials fetched %d times", fresh.calls)
	}
	ending := &fakePortal{lasts: time.Minute}
	p = RoleCredentials(ending, "111111111111", "ElevateAdmin")
	p.Retrieve(ctx)
	p.Retrieve(ctx)
	if ending.calls != 2 {
		t.Errorf("credentials about to expire fetched %d times", ending.calls)
	}
}
