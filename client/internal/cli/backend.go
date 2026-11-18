package cli

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
)

// Service is the part of the api client the commands use.
type Service interface {
	Me(ctx context.Context) (*api.Me, error)
	Rules(ctx context.Context) (*api.RuleList, error)
	RequestableRules(ctx context.Context) (*api.RuleList, error)
	Permissions(ctx context.Context, ruleID string) (*api.Permissions, error)
	PermissionSetPermissions(ctx context.Context, arn string) (*api.Permissions, error)
	CreateGrant(ctx context.Context, req api.CreateRequest) (*api.Grant, error)
	Grant(ctx context.Context, id string) (*api.GrantDetail, error)
	Grants(ctx context.Context, q api.ListQuery) (*api.GrantList, error)
	Approve(ctx context.Context, id string) (*api.Grant, error)
	Reject(ctx context.Context, id, reason string) (*api.Grant, error)
	Cancel(ctx context.Context, id string) (*api.Grant, error)
	Revoke(ctx context.Context, id, reason string) (*api.Grant, error)
	Redrive(ctx context.Context, id string) (*api.Grant, error)
	CheckRules(ctx context.Context, doc []byte) (*api.CheckResult, error)
	ConfigVersions(ctx context.Context) (*api.ConfigVersions, error)
	ConfigVersion(ctx context.Context, n int) (*api.ConfigVersion, error)
}

// Backend is what the commands need from AWS for one source profile.
type Backend interface {
	Source() awsx.SourceProfile
	Discover(ctx context.Context, account string) (awsx.Target, error)
	WaitReady(ctx context.Context, account, role string) error
	Service(ctx context.Context, t awsx.Target) (Service, error)
	Admin(ctx context.Context, t awsx.Target) (AdminAWS, error)
}

// Open is the real Backend: the SSO profile through the AWS SDK.
func Open(ctx context.Context, profile string) (Backend, error) {
	s, cfg, err := awsx.Open(ctx, profile)
	if err != nil {
		return nil, err
	}
	return &sdkBackend{session: s, cfg: cfg}, nil
}

type sdkBackend struct {
	session *awsx.Session
	cfg     aws.Config
}

func (b *sdkBackend) Source() awsx.SourceProfile { return b.session.Profile }

func (b *sdkBackend) Discover(ctx context.Context, account string) (awsx.Target, error) {
	return b.session.Discover(ctx, account)
}

func (b *sdkBackend) WaitReady(ctx context.Context, account, role string) error {
	_, err := b.session.WaitReady(ctx, account, role)
	return err
}

func (b *sdkBackend) Service(ctx context.Context, t awsx.Target) (Service, error) {
	creds, err := b.credentials(ctx, t)
	if err != nil {
		return nil, err
	}
	return api.New(awsx.WithProvider(b.cfg, creds), t.APIURL), nil
}

func (b *sdkBackend) Admin(ctx context.Context, t awsx.Target) (AdminAWS, error) {
	creds, err := b.credentials(ctx, t)
	if err != nil {
		return nil, err
	}
	return awsx.NewAdmin(b.cfg, creds), nil
}

// credentials renew themselves before they expire, for the interface that stays open;
// the first set is read now so a missing role or session fails here, with its cause.
func (b *sdkBackend) credentials(ctx context.Context, t awsx.Target) (aws.CredentialsProvider, error) {
	p := awsx.RoleCredentials(b.session.Portal, t.Account, t.Role)
	if _, err := p.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("get %s credentials in account %s: %w", t.Role, t.Account, err)
	}
	return p, nil
}
