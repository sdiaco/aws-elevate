// Package awsx holds the client's AWS plumbing: the SSO portal, the readiness check of a
// grant, the CLI profile it writes and the client settings. It takes no decision on
// grants: those come from the service.
package awsx

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Service permission sets. Admin wins over Requester, Requester over
// Auditor: the service decides what each user may do from their groups.
const (
	RoleAdmin     = "ElevateAdmin"
	RoleRequester = "ElevateRequester"
	RoleAuditor   = "ElevateAuditor"
)

// ReadyTimeout bounds WaitReady. After SUCCEEDED the portal refuses the credentials for
// 6–7 s and the first call can be denied for 1–2 s more.
const ReadyTimeout = 60 * time.Second

// readyDelays is the client polling backoff: 2, 4, 8 s, then every 8 s.
var readyDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

type PortalAccount struct {
	ID   string
	Name string
}

// Portal is the AWS access portal, called with the user's SSO token.
type Portal interface {
	ListAccounts(ctx context.Context) ([]PortalAccount, error)
	ListRoles(ctx context.Context, account string) ([]string, error)
	Credentials(ctx context.Context, account, role string) (aws.Credentials, error)
}

// Caller makes one signed call with creds, to prove they are usable.
type Caller interface {
	Check(ctx context.Context, creds aws.Credentials) error
}

// Params reads /elevate/api-url with the service role's creds.
type Params interface {
	APIURL(ctx context.Context, creds aws.Credentials) (string, error)
}

// Session is what the client needs from AWS for one source profile.
type Session struct {
	Profile SourceProfile
	Portal  Portal
	Caller  Caller
	Params  Params
	Sleep   func(ctx context.Context, d time.Duration) error
}

// Target is where the service lives for a profile: the service account, the service
// role the user has there and the api address.
type Target struct {
	Account string `json:"account"`
	Role    string `json:"role"`
	APIURL  string `json:"apiUrl"`
}

// Discover finds the account where the user has a service role and reads
// the api address there. account, if not empty, is an id or a name and narrows the search.
func (s *Session) Discover(ctx context.Context, account string) (Target, error) {
	accounts, err := s.Portal.ListAccounts(ctx)
	if err != nil {
		return Target{}, fmt.Errorf("list accounts in the access portal: %w", err)
	}
	type candidate struct {
		account PortalAccount
		role    string
	}
	var found []candidate
	for _, a := range accounts {
		if account != "" && account != a.ID && account != a.Name {
			continue
		}
		roles, err := s.Portal.ListRoles(ctx, a.ID)
		if err != nil {
			return Target{}, fmt.Errorf("list roles in account %s: %w", a.ID, err)
		}
		if role := serviceRole(roles); role != "" {
			found = append(found, candidate{a, role})
		}
	}
	switch len(found) {
	case 0:
		where := "any account"
		if account != "" {
			where = "account " + account
		}
		return Target{}, fmt.Errorf("no %s, %s or %s role in %s of profile %q: ask an administrator for access",
			RoleAdmin, RoleRequester, RoleAuditor, where, s.Profile.Name)
	case 1:
	default:
		names := make([]string, len(found))
		for i, c := range found {
			names[i] = fmt.Sprintf("%s (%s)", c.account.ID, c.account.Name)
		}
		return Target{}, fmt.Errorf("aws-elevate is installed in more than one account: %s; choose one with --account",
			strings.Join(names, ", "))
	}

	c := found[0]
	creds, err := s.Portal.Credentials(ctx, c.account.ID, c.role)
	if err != nil {
		return Target{}, fmt.Errorf("get %s credentials in account %s: %w", c.role, c.account.ID, err)
	}
	url, err := s.Params.APIURL(ctx, creds)
	if err != nil {
		return Target{}, fmt.Errorf("read the api address in account %s: %w", c.account.ID, err)
	}
	return Target{Account: c.account.ID, Role: c.role, APIURL: url}, nil
}

func serviceRole(roles []string) string {
	for _, want := range []string{RoleAdmin, RoleRequester, RoleAuditor} {
		if slices.Contains(roles, want) {
			return want
		}
	}
	return ""
}

// WaitReady retries until the grant's role gives credentials that a real call accepts,
// up to ReadyTimeout (only the client can tell: it holds the SSO token).
func (s *Session) WaitReady(ctx context.Context, account, role string) (aws.Credentials, error) {
	var waited time.Duration
	for attempt := 0; ; attempt++ {
		creds, err := s.Portal.Credentials(ctx, account, role)
		if err == nil {
			err = s.Caller.Check(ctx, creds)
			if err == nil {
				return creds, nil
			}
		}
		delay := readyDelays[min(attempt, len(readyDelays)-1)]
		if waited+delay > ReadyTimeout {
			return aws.Credentials{}, fmt.Errorf("role %s in account %s not usable after %v: %w", role, account, waited, err)
		}
		if serr := s.Sleep(ctx, delay); serr != nil {
			return aws.Credentials{}, errors.Join(serr, err)
		}
		waited += delay
	}
}

// Sleep waits d or until ctx ends.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
