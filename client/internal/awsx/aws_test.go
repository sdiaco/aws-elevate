package awsx

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAWS answers the few SSO portal, STS and SSM calls the client makes.
func fakeAWS(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Sso_bearer_token") != "" && r.Header.Get("X-Amz-Sso_bearer_token") != "tok" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/assignment/accounts":
			fmt.Fprint(w, `{"accountList":[{"accountId":"111111111111","accountName":"shared"}]}`)
		case r.URL.Path == "/assignment/roles":
			fmt.Fprint(w, `{"roleList":[{"roleName":"ElevateRequester","accountId":"`+r.URL.Query().Get("account_id")+`"}]}`)
		case r.URL.Path == "/federation/credentials":
			fmt.Fprint(w, `{"roleCredentials":{"accessKeyId":"AKID","secretAccessKey":"secret","sessionToken":"session","expiration":1893456000000}}`)
		case r.Header.Get("X-Amz-Target") == "AmazonSSM.GetParameter":
			fmt.Fprint(w, `{"Parameter":{"Name":"/elevate/api-url","Value":"https://api.example.com/v1"}}`)
		case r.Method == http.MethodPost:
			_ = r.ParseForm()
			if r.Form.Get("Action") == "GetCallerIdentity" || strings.Contains(r.Header.Get("X-Amz-Target"), "GetCallerIdentity") {
				w.Header().Set("Content-Type", "text/xml")
				fmt.Fprint(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:sts::222222222222:assumed-role/x/user</Arn><UserId>X:user</UserId><Account>222222222222</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
				return
			}
			http.Error(w, "unexpected call", http.StatusBadRequest)
		default:
			t.Logf("unexpected %s %s %v", r.Method, r.URL, r.Header.Get("X-Amz-Target"))
			http.Error(w, "unexpected call "+r.URL.Path, http.StatusBadRequest)
		}
	}))
}

func writeSSOSetup(t *testing.T, configBody string, tokenKey string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".aws", "config")
	if err := os.MkdirAll(filepath.Join(home, ".aws", "sso", "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, ".aws", "credentials"))
	sum := sha1.Sum([]byte(tokenKey))
	token, _ := json.Marshal(map[string]string{
		"accessToken": "tok",
		"expiresAt":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"startUrl":    "https://example.awsapps.com/start",
		"region":      "eu-west-1",
	})
	if err := os.WriteFile(filepath.Join(home, ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json"), token, 0o600); err != nil {
		t.Fatal(err)
	}
}

const sessionConfig = `[profile work]
sso_session = corp
sso_account_id = 111111111111
sso_role_name = ElevateRequester
region = eu-west-1

[sso-session corp]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-west-1
`

func TestOpenDiscoverAndWaitReadyWithTheSDK(t *testing.T) {
	srv := fakeAWS(t)
	defer srv.Close()
	writeSSOSetup(t, sessionConfig, "corp")
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)

	s, cfg, err := Open(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "eu-west-1" {
		t.Errorf("region %q", cfg.Region)
	}
	want := SourceProfile{Name: "work", SSOSession: "corp", SSOStartURL: "https://example.awsapps.com/start", SSORegion: "eu-west-1", Region: "eu-west-1"}
	if s.Profile != want {
		t.Fatalf("profile %+v", s.Profile)
	}
	target, err := s.Discover(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if target != (Target{Account: "111111111111", Role: RoleRequester, APIURL: "https://api.example.com/v1"}) {
		t.Fatalf("target %+v", target)
	}
	creds, err := s.WaitReady(context.Background(), "222222222222", "JIT-abcdefghij")
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessKeyID != "AKID" || creds.SessionToken != "session" || !creds.CanExpire {
		t.Fatalf("creds %+v", creds)
	}
	signed, err := WithCredentials(cfg, creds).Credentials.Retrieve(context.Background())
	if err != nil || signed.AccessKeyID != "AKID" {
		t.Fatalf("WithCredentials: %+v, %v", signed, err)
	}
}

func TestOpenLegacyProfileUsesTheStartURLToken(t *testing.T) {
	writeSSOSetup(t, `[profile old]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-west-1
sso_account_id = 111111111111
sso_role_name = ElevateRequester
region = eu-west-1
`, "https://example.awsapps.com/start")
	s, _, err := Open(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if s.Profile.SSOSession != "" || s.Profile.SSOStartURL != "https://example.awsapps.com/start" {
		t.Fatalf("profile %+v", s.Profile)
	}
}

func TestOpenRefusesProfilesItCannotUse(t *testing.T) {
	writeSSOSetup(t, "[profile keys]\nregion = eu-west-1\n\n[profile noregion]\nsso_session = corp\n\n[sso-session corp]\nsso_start_url = https://example.awsapps.com/start\nsso_region = eu-west-1\n", "corp")
	cases := map[string]string{"keys": "not an IAM Identity Center", "noregion": "no region", "missing": "missing"}
	for profile, msg := range cases {
		_, _, err := Open(context.Background(), profile)
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: got %v, want %q", profile, err, msg)
		}
	}
}

func TestExpiredTokenAsksToLogIn(t *testing.T) {
	srv := fakeAWS(t)
	defer srv.Close()
	writeSSOSetup(t, sessionConfig, "other-key")
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	s, _, err := Open(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Discover(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "aws sso login --profile work") {
		t.Fatalf("got %v", err)
	}
}

func TestSleepStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); err == nil {
		t.Fatal("expected the context error")
	}
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if p, _ := SettingsPath(); p != "/xdg/aws-elevate/config.json" {
		t.Errorf("got %s", p)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if p, _ := SettingsPath(); p != "/home/u/.config/aws-elevate/config.json" {
		t.Errorf("got %s", p)
	}
}
