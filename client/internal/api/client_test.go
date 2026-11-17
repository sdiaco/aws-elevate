package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/sdiaco/aws-elevate/client/internal/version"
)

type seen struct {
	method, path, query, body string
	header                    http.Header
}

// serve answers every call with the given status and body, and records the request.
func serve(t *testing.T, status int, body string) (*Client, *seen) {
	t.Helper()
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*got = seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(raw), r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:      "eu-west-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "token"),
	}
	return New(cfg, srv.URL+"/api"), got
}

func TestCallsAreSignedAndCarryTheClientVersion(t *testing.T) {
	c, got := serve(t, 200, string(loadExample(t, "me").Response))
	me, err := c.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if me.User.UserName != "admin@example.com" || me.Installation == nil || me.Installation.RulesProfileID != "e4f5g6h" {
		t.Errorf("me %+v", me)
	}
	if got.method != "GET" || got.path != "/api/v1/me" {
		t.Errorf("called %s %s", got.method, got.path)
	}
	if auth := got.header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") ||
		!strings.Contains(auth, "/eu-west-1/execute-api/aws4_request") {
		t.Errorf("not signed for execute-api: %q", auth)
	}
	if got.header.Get("X-Amz-Security-Token") != "token" {
		t.Error("session token missing")
	}
	if got.header.Get(VersionHeader) == "" {
		t.Error("client version header missing")
	}
}

func TestServiceErrorIsTyped(t *testing.T) {
	c, _ := serve(t, 404, `{"error": "not_found", "message": "grant zzzzzzzzzz does not exist"}`)
	_, err := c.Grant(context.Background(), "zzzzzzzzzz")
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v", err)
	}
	if apiErr.Status != 404 || apiErr.Code != CodeNotFound || apiErr.Message != "grant zzzzzzzzzz does not exist" {
		t.Errorf("got %+v", apiErr)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("message %q", err.Error())
	}
}

func TestClientTooOld(t *testing.T) {
	c, _ := serve(t, 426, `{"error": "client_too_old", "message": "client 0.0.1 is older than 0.2.0; upgrade aws-elevate"}`)
	_, err := c.Me(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeClientTooOld || apiErr.Status != 426 {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "aws-elevate update") {
		t.Errorf("426 should tell how to upgrade: %q", err.Error())
	}
}

// API Gateway answers its own refusals (resource policy, bad signature) without our format.
func TestGatewayRefusalIsAnError(t *testing.T) {
	c, _ := serve(t, 403, `{"Message": "User is not authorized to access this resource"}`)
	_, err := c.Me(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 403 || apiErr.Code != "" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(apiErr.Message, "not authorized") {
		t.Errorf("message %q", apiErr.Message)
	}
}

func TestUnreadableSuccessIsAnError(t *testing.T) {
	c, _ := serve(t, 200, `not json`)
	if _, err := c.Me(context.Background()); err == nil {
		t.Fatal("no error")
	}
}

func TestCreateSendsTheRequest(t *testing.T) {
	c, got := serve(t, 201, string(loadExample(t, "grants-create").Response))
	g, err := c.CreateGrant(context.Background(), CreateRequest{RuleID: "ssm-prod", Account: "222222222222", Reason: "incident 42"})
	if err != nil {
		t.Fatal(err)
	}
	if g.State != StatePending || g.Actions[0] != "cancel" {
		t.Errorf("got %+v", g)
	}
	// No duration: the service applies the rule default.
	if got.method != "POST" || got.path != "/api/v1/grants" ||
		got.body != `{"ruleId":"ssm-prod","account":"222222222222","reason":"incident 42"}` {
		t.Errorf("sent %s %s %s", got.method, got.path, got.body)
	}
	if got.header.Get("Content-Type") != "application/json" {
		t.Error("content type missing")
	}
}

func TestGrantActions(t *testing.T) {
	calls := []struct {
		name string
		do   func(*Client) (*Grant, error)
		path string
		body string
	}{
		{"approve", func(c *Client) (*Grant, error) { return c.Approve(context.Background(), "abcde23456") }, "/api/v1/grants/abcde23456/approve", ""},
		{"reject", func(c *Client) (*Grant, error) { return c.Reject(context.Background(), "abcde23456") }, "/api/v1/grants/abcde23456/reject", ""},
		{"cancel", func(c *Client) (*Grant, error) { return c.Cancel(context.Background(), "abcde23456") }, "/api/v1/grants/abcde23456/cancel", ""},
		{"revoke", func(c *Client) (*Grant, error) {
			return c.Revoke(context.Background(), "abcde23456", "incident closed")
		}, "/api/v1/grants/abcde23456/revoke", `{"reason":"incident closed"}`},
		{"redrive", func(c *Client) (*Grant, error) { return c.Redrive(context.Background(), "abcde23456") }, "/api/v1/grants/abcde23456/redrive", ""},
	}
	for _, call := range calls {
		c, got := serve(t, 200, string(loadExample(t, "grants-"+call.name).Response))
		g, err := call.do(c)
		if err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
		if g.ID != "abcde23456" || got.method != "POST" || got.path != call.path || got.body != call.body {
			t.Errorf("%s: sent %s %s %q", call.name, got.method, got.path, got.body)
		}
	}
}

func TestGrantDetail(t *testing.T) {
	c, got := serve(t, 200, string(loadExample(t, "grants-get").Response))
	d, err := c.Grant(context.Background(), "abcde23456")
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/v1/grants/abcde23456" || d.Grant.ExpiresAt == nil || len(d.Timeline) != 1 {
		t.Errorf("got %+v from %s", d, got.path)
	}
}

func TestListSendsTheQuery(t *testing.T) {
	c, got := serve(t, 200, string(loadExample(t, "grants-list").Response))
	l, err := c.Grants(context.Background(), ListQuery{Scope: "all", Open: true, User: "user@example.com", Cursor: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(got.query)
	if q.Get("scope") != "all" || q.Get("open") != "true" || q.Get("user") != "user@example.com" || q.Get("cursor") != "abc" {
		t.Errorf("query %q", got.query)
	}
	if len(l.Items) != 2 || l.Cursor != nil {
		t.Errorf("got %+v", l)
	}
	c, got = serve(t, 200, string(loadExample(t, "grants-list").Response))
	if _, err := c.Grants(context.Background(), ListQuery{Scope: "mine", Closed: true}); err != nil {
		t.Fatal(err)
	}
	if q, _ := url.ParseQuery(got.query); q.Get("closed") != "true" || q.Has("open") {
		t.Errorf("closed query %q", got.query)
	}
	c, got = serve(t, 200, string(loadExample(t, "grants-list").Response))
	if _, err := c.Grants(context.Background(), ListQuery{}); err != nil {
		t.Fatal(err)
	}
	if got.query != "" {
		t.Errorf("empty query sent as %q", got.query)
	}
}

func TestRulesAndPermissions(t *testing.T) {
	c, got := serve(t, 200, string(loadExample(t, "rules-list").Response))
	rules, err := c.Rules(context.Background())
	if err != nil || len(rules.Rules) != 2 || got.path != "/api/v1/rules" {
		t.Fatalf("got %+v %v from %s", rules, err, got.path)
	}
	if got.query != "" {
		t.Errorf("Rules sent %q", got.query)
	}
	c, got = serve(t, 200, string(loadExample(t, "rules-list").Response))
	if _, err := c.RequestableRules(context.Background()); err != nil || got.query != "scope=requestable" {
		t.Fatalf("RequestableRules sent %q: %v", got.query, err)
	}
	c, got = serve(t, 200, string(loadExample(t, "rules-permissions").Response))
	p, err := c.Permissions(context.Background(), "ssm dev/x")
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/v1/rules/ssm%20dev%2Fx/permissions" {
		t.Errorf("rule id not escaped: %s", got.path)
	}
	if p.Boundary == nil || p.Boundary.ManagedPolicyArn == nil {
		t.Errorf("boundary %+v", p.Boundary)
	}
	arn := "arn:aws:sso:::permissionSet/ssoins-1/ps-1"
	c, got = serve(t, 200, string(loadExample(t, "permission-sets-permissions").Response))
	if _, err := c.PermissionSetPermissions(context.Background(), arn); err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/v1/permission-sets/permissions" || got.query != (url.Values{"arn": {arn}}).Encode() {
		t.Errorf("sent %s?%s", got.path, got.query)
	}
}

func TestCheckRules(t *testing.T) {
	c, got := serve(t, 200, string(loadExample(t, "rules-check").Response))
	doc := []byte(`{"schemaVersion": 1, "rules": []}`)
	r, err := c.CheckRules(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if got.body != `{"doc":{"schemaVersion":1,"rules":[]}}` {
		t.Errorf("sent %s", got.body)
	}
	if r.Valid || r.Errors[0].Code != "ps_not_found" {
		t.Errorf("got %+v", r)
	}
	if _, err := c.CheckRules(context.Background(), []byte(`{`)); err == nil {
		t.Error("invalid JSON document sent")
	}
}

func TestConfigVersions(t *testing.T) {
	c, got := serve(t, 200, string(loadExample(t, "config-versions").Response))
	v, err := c.ConfigVersions(context.Background())
	if err != nil || v.Current == nil || *v.Current != 3 || got.path != "/api/v1/config/versions" {
		t.Fatalf("got %+v %v", v, err)
	}
	c, got = serve(t, 200, string(loadExample(t, "config-version").Response))
	one, err := c.ConfigVersion(context.Background(), 3)
	if err != nil || one.Version.Number != 3 || got.path != "/api/v1/config/versions/3" {
		t.Fatalf("got %+v %v", one, err)
	}
}

func TestCredentialFailureIsAnError(t *testing.T) {
	cfg := aws.Config{Region: "eu-west-1", Credentials: aws.CredentialsProviderFunc(
		func(context.Context) (aws.Credentials, error) { return aws.Credentials{}, errors.New("token expired") })}
	_, err := New(cfg, "http://127.0.0.1:1").Me(context.Background())
	if err == nil || !strings.Contains(err.Error(), "token expired") {
		t.Fatalf("got %v", err)
	}
}

func TestUnreachableServiceIsAnError(t *testing.T) {
	c, _ := serve(t, 200, "{}")
	c.base = "http://127.0.0.1:1"
	if _, err := c.Me(context.Background()); err == nil {
		t.Fatal("no error")
	}
}

func serveWithInfra(t *testing.T, infra string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(InfraVersionHeader, infra)
		w.Header().Set(MinClientHeader, "0.0.1")
		_, _ = io.WriteString(w, string(loadExample(t, "me").Response))
	}))
	t.Cleanup(srv.Close)
	cfg := aws.Config{Region: "eu-west-1", Credentials: credentials.NewStaticCredentialsProvider("a", "b", "c")}
	return New(cfg, srv.URL)
}

func TestInfraTooOldStopsTheClient(t *testing.T) {
	old := version.MinInfra
	version.MinInfra = "0.2.0"
	t.Cleanup(func() { version.MinInfra = old })

	_, err := serveWithInfra(t, "0.1.9").Me(context.Background())
	if !errors.Is(err, ErrInfraTooOld) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "0.1.9") || !strings.Contains(err.Error(), "0.2.0") {
		t.Errorf("message %q", err.Error())
	}
}

func TestCompatibleInfraPasses(t *testing.T) {
	if _, err := serveWithInfra(t, version.MinInfra).Me(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUnreadableInfraVersionIsAnError(t *testing.T) {
	if _, err := serveWithInfra(t, "latest").Me(context.Background()); err == nil {
		t.Fatal("no error")
	}
}

// update needs the infrastructure version even when the service refuses this client.
func TestClientTooOldCarriesTheInfraVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(InfraVersionHeader, "0.3.0")
		w.Header().Set(MinClientHeader, "0.3.0")
		w.WriteHeader(426)
		_, _ = io.WriteString(w, `{"error": "client_too_old", "message": "upgrade aws-elevate"}`)
	}))
	t.Cleanup(srv.Close)
	cfg := aws.Config{Region: "eu-west-1", Credentials: credentials.NewStaticCredentialsProvider("a", "b", "c")}
	_, err := New(cfg, srv.URL).Me(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.InfraVersion != "0.3.0" {
		t.Fatalf("got %#v", err)
	}
}
