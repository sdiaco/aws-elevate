package awsx

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/identitystore"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/ssoadmin"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Installation is the account and Identity Center instance the stack goes into.
type Installation struct {
	Account         string
	IdentityStoreID string
}

type Group struct {
	ID   string
	Name string
}

// NewStack is a CreateStack request. Parameters left out take the template defaults.
type NewStack struct {
	Name        string
	TemplateURL string
	Parameters  map[string]string
}

// StackEvent is one line of the stack's progress.
type StackEvent struct {
	ID       string
	Resource string
	Status   string
	Reason   string
}

// StackState is the stack status and its latest events, oldest first.
type StackState struct {
	Status string
	Events []StackEvent
}

// Installer makes the calls of the guided install with the admin's own
// profile, in the region chosen for the stack.
type Installer struct{ cfg aws.Config }

// OpenInstaller signs with the credentials of profile, in region.
func OpenInstaller(ctx context.Context, profile, region string) (*Installer, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load profile %q: %w", profile, err)
	}
	return &Installer{cfg: cfg}, nil
}

// RegisterHint tells how to make account the Identity Center delegated administrator,
// the only organization prerequisite.
func RegisterHint(account string) string {
	return "To make it the delegated administrator, run once from the management account: " +
		"aws organizations register-delegated-administrator --account-id " + account + " --service-principal sso.amazonaws.com"
}

// Check makes the precheck of the stack before creating it, for a faster and clearer
// answer: the region must hold the Identity Center instance and the account must be its
// delegated administrator. The stack checks again on its own. When a check fails the
// answer still carries the account, which is known by then.
func (i *Installer) Check(ctx context.Context) (Installation, error) {
	id, err := sts.NewFromConfig(i.cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return Installation{}, fmt.Errorf("read the account of the profile: %w", err)
	}
	account := aws.ToString(id.Account)
	sso := ssoadmin.NewFromConfig(i.cfg)
	inst, err := sso.ListInstances(ctx, &ssoadmin.ListInstancesInput{})
	if err != nil {
		return Installation{Account: account}, fmt.Errorf("list Identity Center instances in %s: %w", i.cfg.Region, err)
	}
	if len(inst.Instances) == 0 {
		return Installation{Account: account}, fmt.Errorf("no IAM Identity Center instance visible in %s from account %s: "+
			"use the Identity Center home region (--region) and its delegated administrator account. %s", i.cfg.Region, account, RegisterHint(account))
	}
	org, err := organizations.NewFromConfig(i.cfg).DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	if err != nil {
		return Installation{Account: account}, fmt.Errorf("read the organization: %w", err)
	}
	if aws.ToString(org.Organization.MasterAccountId) == account {
		return Installation{Account: account}, fmt.Errorf("account %s is the management account: install in the Identity Center delegated administrator account", account)
	}
	_, err = sso.ListPermissionSets(ctx, &ssoadmin.ListPermissionSetsInput{InstanceArn: inst.Instances[0].InstanceArn, MaxResults: aws.Int32(1)})
	if err != nil {
		return Installation{Account: account}, fmt.Errorf("account %s is not the Identity Center delegated administrator, or the profile cannot administer Identity Center (%w). %s", account, err, RegisterHint(account))
	}
	return Installation{Account: account, IdentityStoreID: aws.ToString(inst.Instances[0].IdentityStoreId)}, nil
}

// Groups lists the groups of the identity store.
func (i *Installer) Groups(ctx context.Context, identityStoreID string) ([]Group, error) {
	var groups []Group
	pages := identitystore.NewListGroupsPaginator(identitystore.NewFromConfig(i.cfg), &identitystore.ListGroupsInput{IdentityStoreId: &identityStoreID})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list Identity Center groups: %w", err)
		}
		for _, g := range page.Groups {
			groups = append(groups, Group{ID: aws.ToString(g.GroupId), Name: aws.ToString(g.DisplayName)})
		}
	}
	return groups, nil
}

// CreateStack starts the stack and returns its id. A failed creation deletes the stack,
// so the install can simply be run again; its events stay readable by id.
func (i *Installer) CreateStack(ctx context.Context, s NewStack) (string, error) {
	in := &cloudformation.CreateStackInput{
		StackName:    &s.Name,
		TemplateURL:  &s.TemplateURL,
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityNamedIam},
		OnFailure:    cfntypes.OnFailureDelete,
	}
	keys := make([]string, 0, len(s.Parameters))
	for k := range s.Parameters {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		in.Parameters = append(in.Parameters, cfntypes.Parameter{ParameterKey: aws.String(k), ParameterValue: aws.String(s.Parameters[k])})
	}
	out, err := cloudformation.NewFromConfig(i.cfg).CreateStack(ctx, in)
	var exists *cfntypes.AlreadyExistsException
	if errors.As(err, &exists) {
		return "", fmt.Errorf("stack %s already exists in %s: to update it see aws elevate admin updates", s.Name, i.cfg.Region)
	}
	if err != nil {
		return "", fmt.Errorf("create stack %s: %w", s.Name, err)
	}
	return aws.ToString(out.StackId), nil
}

// TemplateResources counts the resources of the template, conditional ones included: the
// total of the install progress.
func (i *Installer) TemplateResources(ctx context.Context, templateURL string) (int, error) {
	out, err := cloudformation.NewFromConfig(i.cfg).GetTemplateSummary(ctx, &cloudformation.GetTemplateSummaryInput{TemplateURL: &templateURL})
	if err != nil {
		return 0, fmt.Errorf("read the template %s: %w", templateURL, err)
	}
	return len(out.ResourceTypes), nil
}

// Stack reads the status and the latest page of events. A page is enough between two
// polls: the stack never logs that many events in a few seconds.
func (i *Installer) Stack(ctx context.Context, stackID string) (StackState, error) {
	client := cloudformation.NewFromConfig(i.cfg)
	stacks, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &stackID})
	if err != nil {
		return StackState{}, fmt.Errorf("read stack: %w", err)
	}
	if len(stacks.Stacks) == 0 {
		return StackState{}, fmt.Errorf("stack %s not found", stackID)
	}
	events, err := client.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: &stackID})
	if err != nil {
		return StackState{}, fmt.Errorf("read stack events: %w", err)
	}
	state := StackState{Status: string(stacks.Stacks[0].StackStatus)}
	for _, e := range slices.Backward(events.StackEvents) {
		state.Events = append(state.Events, StackEvent{
			ID:       aws.ToString(e.EventId),
			Resource: aws.ToString(e.LogicalResourceId),
			Status:   string(e.ResourceStatus),
			Reason:   aws.ToString(e.ResourceStatusReason),
		})
	}
	return state, nil
}
