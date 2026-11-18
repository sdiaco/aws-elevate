package awsx

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sso"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// APIURLParameter is written by the stack.
const APIURLParameter = "/elevate/api-url"

// ConfigFile is the AWS CLI config file: $AWS_CONFIG_FILE, else ~/.aws/config.
func ConfigFile() string {
	if f := os.Getenv("AWS_CONFIG_FILE"); f != "" {
		return f
	}
	return config.DefaultSharedConfigFilename()
}

// ReadProfile reads how profile signs in. SSOStartURL is empty for a profile that is not
// an IAM Identity Center (SSO) profile.
func ReadProfile(ctx context.Context, profile string) (SourceProfile, error) {
	shared, err := config.LoadSharedConfigProfile(ctx, profile, func(o *config.LoadSharedConfigOptions) {
		o.ConfigFiles = []string{ConfigFile()}
	})
	if err != nil {
		return SourceProfile{}, fmt.Errorf("read profile %q: %w", profile, err)
	}
	src := SourceProfile{Name: profile, SSOStartURL: shared.SSOStartURL, SSORegion: shared.SSORegion, Region: shared.Region}
	if shared.SSOSession != nil {
		src.SSOSession = shared.SSOSession.Name
		src.SSOStartURL = shared.SSOSession.SSOStartURL
		src.SSORegion = shared.SSOSession.SSORegion
	}
	return src, nil
}

// Open reads the SSO profile and wires the real AWS calls. The SSO token comes from the
// cache of `aws sso login` and is refreshed there by the SDK.
func Open(ctx context.Context, profile string) (*Session, aws.Config, error) {
	src, err := ReadProfile(ctx, profile)
	if err != nil {
		return nil, aws.Config{}, err
	}
	tokenKey := src.SSOStartURL
	if src.SSOSession != "" {
		tokenKey = src.SSOSession
	}
	if src.SSOStartURL == "" || src.SSORegion == "" {
		return nil, aws.Config{}, fmt.Errorf("profile %q is not an IAM Identity Center (SSO) profile: configure it with aws configure sso", profile)
	}
	if src.Region == "" {
		return nil, aws.Config{}, fmt.Errorf("profile %q has no region: add region = <region of aws-elevate>", profile)
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile))
	if err != nil {
		return nil, aws.Config{}, fmt.Errorf("load profile %q: %w", profile, err)
	}
	tokenFile, err := ssocreds.StandardCachedTokenFilepath(tokenKey)
	if err != nil {
		return nil, aws.Config{}, fmt.Errorf("find the SSO token cache: %w", err)
	}
	inSSORegion := func(o *ssooidc.Options) { o.Region = src.SSORegion }
	tokens := ssocreds.NewSSOTokenProvider(ssooidc.NewFromConfig(cfg, inSSORegion), tokenFile)

	s := &Session{
		Profile: src,
		Portal:  &portal{client: sso.NewFromConfig(cfg, func(o *sso.Options) { o.Region = src.SSORegion }), tokens: tokens, profile: profile},
		Caller:  caller{cfg: cfg},
		Params:  params{cfg: cfg},
		Sleep:   Sleep,
	}
	return s, cfg, nil
}

// WithCredentials returns cfg signing with creds, for one short call.
func WithCredentials(cfg aws.Config, creds aws.Credentials) aws.Config {
	return WithProvider(cfg, static(creds))
}

// WithProvider returns cfg signing with the credentials of p.
func WithProvider(cfg aws.Config, p aws.CredentialsProvider) aws.Config {
	cfg = cfg.Copy()
	cfg.Credentials = p
	return cfg
}

// credentialsMargin is how long before expiry role credentials are fetched again, so a
// call never leaves with credentials that expire on the way.
const credentialsMargin = 5 * time.Minute

// RoleCredentials are the credentials of role in account, fetched from the portal again
// before they expire: the interface stays open for hours, longer than one set lasts.
func RoleCredentials(p Portal, account, role string) aws.CredentialsProvider {
	fetch := aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return p.Credentials(ctx, account, role)
	})
	return aws.NewCredentialsCache(fetch, func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = credentialsMargin })
}

func static(c aws.Credentials) aws.CredentialsProvider {
	return credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
}

// SessionExpiredError is an SSO session that has expired or was never opened: only the
// user can open it again, with Command.
type SessionExpiredError struct {
	Profile string
	Err     error
}

func (e *SessionExpiredError) Command() string { return "aws sso login --profile " + e.Profile }

func (e *SessionExpiredError) Error() string {
	return fmt.Sprintf("SSO session expired or missing: run %s (%v)", e.Command(), e.Err)
}

func (e *SessionExpiredError) Unwrap() error { return e.Err }

type portal struct {
	client  *sso.Client
	tokens  *ssocreds.SSOTokenProvider
	profile string
}

func (p *portal) token(ctx context.Context) (*string, error) {
	t, err := p.tokens.RetrieveBearerToken(ctx)
	if err != nil {
		return nil, &SessionExpiredError{Profile: p.profile, Err: err}
	}
	return &t.Value, nil
}

func (p *portal) ListAccounts(ctx context.Context) ([]PortalAccount, error) {
	token, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	var out []PortalAccount
	pages := sso.NewListAccountsPaginator(p.client, &sso.ListAccountsInput{AccessToken: token})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, a := range page.AccountList {
			out = append(out, PortalAccount{ID: aws.ToString(a.AccountId), Name: aws.ToString(a.AccountName)})
		}
	}
	return out, nil
}

func (p *portal) ListRoles(ctx context.Context, account string) ([]string, error) {
	token, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	pages := sso.NewListAccountRolesPaginator(p.client, &sso.ListAccountRolesInput{AccessToken: token, AccountId: &account})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.RoleList {
			out = append(out, aws.ToString(r.RoleName))
		}
	}
	return out, nil
}

func (p *portal) Credentials(ctx context.Context, account, role string) (aws.Credentials, error) {
	token, err := p.token(ctx)
	if err != nil {
		return aws.Credentials{}, err
	}
	out, err := p.client.GetRoleCredentials(ctx, &sso.GetRoleCredentialsInput{AccessToken: token, AccountId: &account, RoleName: &role})
	if err != nil {
		return aws.Credentials{}, err
	}
	c := out.RoleCredentials
	return aws.Credentials{
		AccessKeyID:     aws.ToString(c.AccessKeyId),
		SecretAccessKey: aws.ToString(c.SecretAccessKey),
		SessionToken:    aws.ToString(c.SessionToken),
		CanExpire:       true,
		Expires:         time.UnixMilli(c.Expiration),
	}, nil
}

type caller struct{ cfg aws.Config }

func (c caller) Check(ctx context.Context, creds aws.Credentials) error {
	_, err := sts.NewFromConfig(WithCredentials(c.cfg, creds)).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	return err
}

type params struct{ cfg aws.Config }

func (p params) APIURL(ctx context.Context, creds aws.Credentials) (string, error) {
	out, err := ssm.NewFromConfig(WithCredentials(p.cfg, creds)).GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(APIURLParameter)})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Parameter.Value), nil
}
