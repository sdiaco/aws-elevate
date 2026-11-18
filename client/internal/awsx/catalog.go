package awsx

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/identitystore"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/aws/aws-sdk-go-v2/service/ssoadmin"
	ssotypes "github.com/aws/aws-sdk-go-v2/service/ssoadmin/types"
)

// Named is something a rule points to: a permission set (ID is its ARN), an account or a
// group.
type Named struct {
	ID   string
	Name string
}

func byName(items []Named) []Named {
	slices.SortFunc(items, func(x, y Named) int { return strings.Compare(x.Name, y.Name) })
	return items
}

func (a *Admin) instance(ctx context.Context) (ssotypes.InstanceMetadata, error) {
	out, err := ssoadmin.NewFromConfig(a.cfg).ListInstances(ctx, &ssoadmin.ListInstancesInput{})
	if err != nil {
		return ssotypes.InstanceMetadata{}, err
	}
	if len(out.Instances) == 0 {
		return ssotypes.InstanceMetadata{}, ErrNoInstance
	}
	return out.Instances[0], nil
}

// ErrNoInstance means the account sees no Identity Center instance: in the service
// account, it is no longer the delegated administrator.
var ErrNoInstance = errors.New("no IAM Identity Center instance visible from this account")

// PermissionSets are the permission sets of the Identity Center instance (design §7.5).
func (a *Admin) PermissionSets(ctx context.Context) ([]Named, error) {
	inst, err := a.instance(ctx)
	if err != nil {
		return nil, err
	}
	client := ssoadmin.NewFromConfig(a.cfg)
	var out []Named
	pages := ssoadmin.NewListPermissionSetsPaginator(client, &ssoadmin.ListPermissionSetsInput{InstanceArn: inst.InstanceArn})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, ps := range page.PermissionSets {
			d, err := client.DescribePermissionSet(ctx, &ssoadmin.DescribePermissionSetInput{InstanceArn: inst.InstanceArn, PermissionSetArn: aws.String(ps)})
			if err != nil {
				return nil, err
			}
			out = append(out, Named{ID: ps, Name: aws.ToString(d.PermissionSet.Name)})
		}
	}
	return byName(out), nil
}

// Accounts are the active accounts of the organization except the management account,
// which a rule never offers (design §3.1). The management account id is the account
// field of every organizations ARN, so no other call is needed.
func (a *Admin) Accounts(ctx context.Context) ([]Named, error) {
	var out []Named
	pages := organizations.NewListAccountsPaginator(organizations.NewFromConfig(a.cfg), &organizations.ListAccountsInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, acc := range page.Accounts {
			id := aws.ToString(acc.Id)
			if acc.Status != orgtypes.AccountStatusActive || managementOf(aws.ToString(acc.Arn)) == id {
				continue
			}
			out = append(out, Named{ID: id, Name: aws.ToString(acc.Name)})
		}
	}
	return byName(out), nil
}

// managementOf reads arn:aws:organizations::<management>:account/o-…/<id>.
func managementOf(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 5 {
		return ""
	}
	return parts[4]
}

// Groups are the groups of the Identity Center identity store.
func (a *Admin) Groups(ctx context.Context) ([]Named, error) {
	inst, err := a.instance(ctx)
	if err != nil {
		return nil, err
	}
	var out []Named
	pages := identitystore.NewListGroupsPaginator(identitystore.NewFromConfig(a.cfg), &identitystore.ListGroupsInput{IdentityStoreId: inst.IdentityStoreId})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, g := range page.Groups {
			out = append(out, Named{ID: aws.ToString(g.GroupId), Name: aws.ToString(g.DisplayName)})
		}
	}
	return byName(out), nil
}
