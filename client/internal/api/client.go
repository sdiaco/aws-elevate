// Package api calls the aws-elevate service: SigV4-signed HTTPS requests to API Gateway
// (design §8.1). It carries no business rule: decisions come back from the service.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// VersionHeader carries the client version; the service answers 426 below its minimum.
const VersionHeader = "X-Elevate-Client-Version"

// Every answer of the service carries these (design §12.2).
const (
	InfraVersionHeader = "X-Elevate-Infra-Version"
	MinClientHeader    = "X-Elevate-Min-Client"
)

// ErrInfraTooOld: the infrastructure is older than this client's minimum.
var ErrInfraTooOld = errors.New("infrastructure too old for this client")

// Error is an answer of the service other than success. Code is empty when the refusal
// comes from API Gateway itself (resource policy, signature), not from the service.
type Error struct {
	Status  int
	Code    string
	Message string
	// InfraVersion is the version the service declared in its answer, if any.
	InfraVersion string
}

func (e *Error) Error() string {
	if e.Code == CodeClientTooOld {
		return e.Message + " (run: aws-elevate update)"
	}
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type Client struct {
	cfg    aws.Config
	base   string
	http   *http.Client
	signer *v4.Signer
}

// New returns a client for the api at baseURL (the value of /elevate/api-url), signing
// with the credentials and region of cfg.
func New(cfg aws.Config, baseURL string) *Client {
	return &Client{cfg: cfg, base: baseURL, http: &http.Client{Timeout: 30 * time.Second}, signer: v4.NewSigner()}
}

func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	return &out, c.do(ctx, http.MethodGet, "/v1/me", nil, nil, &out)
}

func (c *Client) CreateGrant(ctx context.Context, req CreateRequest) (*Grant, error) {
	var out GrantResponse
	return &out.Grant, c.do(ctx, http.MethodPost, "/v1/grants", nil, req, &out)
}

func (c *Client) Grants(ctx context.Context, q ListQuery) (*GrantList, error) {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("scope", q.Scope)
	if q.Open {
		v.Set("open", "true")
	}
	set("user", q.User)
	set("from", q.From)
	set("to", q.To)
	set("cursor", q.Cursor)
	var out GrantList
	return &out, c.do(ctx, http.MethodGet, "/v1/grants", v, nil, &out)
}

func (c *Client) Grant(ctx context.Context, id string) (*GrantDetail, error) {
	var out GrantDetail
	return &out, c.do(ctx, http.MethodGet, "/v1/grants/"+url.PathEscape(id), nil, nil, &out)
}

func (c *Client) Approve(ctx context.Context, id string) (*Grant, error) {
	return c.action(ctx, id, "approve", nil)
}

func (c *Client) Reject(ctx context.Context, id string) (*Grant, error) {
	return c.action(ctx, id, "reject", nil)
}

func (c *Client) Cancel(ctx context.Context, id string) (*Grant, error) {
	return c.action(ctx, id, "cancel", nil)
}

func (c *Client) Revoke(ctx context.Context, id, reason string) (*Grant, error) {
	return c.action(ctx, id, "revoke", revokeRequest{Reason: reason})
}

func (c *Client) Redrive(ctx context.Context, id string) (*Grant, error) {
	return c.action(ctx, id, "redrive", nil)
}

func (c *Client) Rules(ctx context.Context) (*RuleList, error) {
	var out RuleList
	return &out, c.do(ctx, http.MethodGet, "/v1/rules", nil, nil, &out)
}

func (c *Client) Permissions(ctx context.Context, ruleID string) (*Permissions, error) {
	var out Permissions
	return &out, c.do(ctx, http.MethodGet, "/v1/rules/"+url.PathEscape(ruleID)+"/permissions", nil, nil, &out)
}

// CheckRules asks the service to check a rules document before it is published.
func (c *Client) CheckRules(ctx context.Context, doc []byte) (*CheckResult, error) {
	if !json.Valid(doc) {
		return nil, fmt.Errorf("the rules document is not valid JSON")
	}
	var out CheckResult
	body := struct {
		Doc json.RawMessage `json:"doc"`
	}{doc}
	return &out, c.do(ctx, http.MethodPost, "/v1/rules/check", nil, body, &out)
}

func (c *Client) ConfigVersions(ctx context.Context) (*ConfigVersions, error) {
	var out ConfigVersions
	return &out, c.do(ctx, http.MethodGet, "/v1/config/versions", nil, nil, &out)
}

func (c *Client) ConfigVersion(ctx context.Context, n int) (*ConfigVersion, error) {
	var out ConfigVersion
	return &out, c.do(ctx, http.MethodGet, "/v1/config/versions/"+strconv.Itoa(n), nil, nil, &out)
}

func (c *Client) action(ctx context.Context, id, name string, body any) (*Grant, error) {
	var out GrantResponse
	return &out.Grant, c.do(ctx, http.MethodPost, "/v1/grants/"+url.PathEscape(id)+"/"+name, nil, body, &out)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set(VersionHeader, version.Client)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	creds, err := c.cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("aws credentials: %w", err)
	}
	sum := sha256.Sum256(payload)
	if err := c.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "execute-api", c.cfg.Region, time.Now()); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call aws-elevate: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	if err := checkInfra(resp.Header); err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		e := decodeError(resp.StatusCode, raw)
		e.InfraVersion = resp.Header.Get(InfraVersionHeader)
		return e
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unreadable answer from aws-elevate (HTTP %d): %w", resp.StatusCode, err)
	}
	return nil
}

// checkInfra stops the client on an infrastructure older than its minimum. Answers of API
// Gateway itself carry no version and are not checked.
func checkInfra(h http.Header) error {
	infra, minClient := h.Get(InfraVersionHeader), h.Get(MinClientHeader)
	if infra == "" || minClient == "" {
		return nil
	}
	outcome, err := version.Compare(version.Client, infra, minClient, version.MinInfra)
	if err != nil {
		return fmt.Errorf("unreadable version in the answer of aws-elevate: %w", err)
	}
	if outcome == version.InfraTooOld {
		return fmt.Errorf("%w: the infrastructure is %s, this client needs at least %s; "+
			"ask an administrator to update the stack (aws elevate admin updates)", ErrInfraTooOld, infra, version.MinInfra)
	}
	return nil
}

func decodeError(status int, raw []byte) *Error {
	var ours errorBody
	if json.Unmarshal(raw, &ours) == nil && ours.Error != "" {
		return &Error{Status: status, Code: ours.Error, Message: ours.Message}
	}
	var gateway struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &gateway) == nil && gateway.Message != "" {
		return &Error{Status: status, Message: gateway.Message}
	}
	return &Error{Status: status, Message: string(raw)}
}
