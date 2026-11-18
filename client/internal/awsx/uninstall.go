package awsx

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfig"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ssoadmin"
	ssotypes "github.com/aws/aws-sdk-go-v2/service/ssoadmin/types"
	"github.com/aws/smithy-go"
)

// Leftovers is what of an installation still exists in the account. Every field is read
// again on each run, so an interrupted uninstall continues where it stopped.
type Leftovers struct {
	Stack     *StackInfo // nil when the stack is gone
	Table     bool       // the grants table <stack>-grants
	RulesApp  string     // id of the AppConfig application <stack>, "" when gone
	ServicePS []string   // ElevateRequester, ElevateAdmin, ElevateAuditor still present
	Orphans   []OrphanPS // JIT-* permission sets of grants, tagged jit=true
}

// Empty says that nothing of the installation is left in the account.
func (l Leftovers) Empty() bool {
	return l.Stack == nil && !l.Table && l.RulesApp == "" && len(l.ServicePS) == 0 && len(l.Orphans) == 0
}

type StackInfo struct {
	ID        string
	Status    string
	Resources int
}

// OrphanPS is a permission set the engine made for a grant, with where it is assigned.
type OrphanPS struct {
	ARN         string
	Name        string
	Assignments []Assignment
}

type Assignment struct{ Account, PrincipalType, PrincipalID string }

// servicePermissionSets are the permission sets the stack creates, by their fixed names.
var servicePermissionSets = []string{RoleRequester, RoleAdmin, RoleAuditor}

// Uninstaller makes the calls of the guided uninstall with the account admin's own
// profile, in the region of the stack.
type Uninstaller struct {
	*Installer
	stack string
	sleep func(context.Context, time.Duration) error
}

// OpenUninstaller signs with the credentials of profile, in region, for the stack named stack.
func OpenUninstaller(ctx context.Context, profile, region, stack string) (*Uninstaller, error) {
	inst, err := OpenInstaller(ctx, profile, region)
	if err != nil {
		return nil, err
	}
	return &Uninstaller{Installer: inst, stack: stack, sleep: sleepCtx}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (u *Uninstaller) tableName() string { return u.stack + "-grants" }

// Leftovers reads what is left: the stack, the retained table and rules, the service
// permission sets and the JIT-* permission sets with their assignments.
func (u *Uninstaller) Leftovers(ctx context.Context) (Leftovers, error) {
	var l Leftovers
	var err error
	if l.Stack, err = u.stackInfo(ctx); err != nil {
		return l, err
	}
	if l.Table, err = u.tableExists(ctx); err != nil {
		return l, err
	}
	if l.RulesApp, err = u.rulesApp(ctx); err != nil {
		return l, err
	}
	l.ServicePS, l.Orphans, err = u.permissionSets(ctx)
	return l, err
}

// notFound says whether err is a ValidationError that the stack does not exist: the only
// way CloudFormation answers for a stack name that is gone.
func notFound(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "ValidationError" && strings.Contains(api.ErrorMessage(), "does not exist")
}

func (u *Uninstaller) stackInfo(ctx context.Context) (*StackInfo, error) {
	client := cloudformation.NewFromConfig(u.cfg)
	out, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &u.stack})
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read stack %s: %w", u.stack, err)
	}
	if len(out.Stacks) == 0 {
		return nil, nil
	}
	s := out.Stacks[0]
	info := &StackInfo{ID: aws.ToString(s.StackId), Status: string(s.StackStatus)}
	pages := cloudformation.NewListStackResourcesPaginator(client, &cloudformation.ListStackResourcesInput{StackName: s.StackId})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the resources of stack %s: %w", u.stack, err)
		}
		info.Resources += len(page.StackResourceSummaries)
	}
	return info, nil
}

func (u *Uninstaller) tableExists(ctx context.Context) (bool, error) {
	name := u.tableName()
	_, err := dynamodb.NewFromConfig(u.cfg).DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &name})
	var missing *ddbtypes.ResourceNotFoundException
	if errors.As(err, &missing) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read table %s: %w", name, err)
	}
	return true, nil
}

func (u *Uninstaller) rulesApp(ctx context.Context) (string, error) {
	pages := appconfig.NewListApplicationsPaginator(appconfig.NewFromConfig(u.cfg), &appconfig.ListApplicationsInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("list AppConfig applications: %w", err)
		}
		for _, app := range page.Items {
			if aws.ToString(app.Name) == u.stack {
				return aws.ToString(app.Id), nil
			}
		}
	}
	return "", nil
}

func (u *Uninstaller) instanceARN(ctx context.Context) (string, error) {
	out, err := ssoadmin.NewFromConfig(u.cfg).ListInstances(ctx, &ssoadmin.ListInstancesInput{})
	if err != nil {
		return "", fmt.Errorf("list Identity Center instances: %w", err)
	}
	if len(out.Instances) == 0 {
		return "", fmt.Errorf("no IAM Identity Center instance visible in %s", u.cfg.Region)
	}
	return aws.ToString(out.Instances[0].InstanceArn), nil
}

func (u *Uninstaller) permissionSets(ctx context.Context) ([]string, []OrphanPS, error) {
	instance, err := u.instanceARN(ctx)
	if err != nil {
		return nil, nil, err
	}
	sso := ssoadmin.NewFromConfig(u.cfg)
	var service []string
	var orphans []OrphanPS
	pages := ssoadmin.NewListPermissionSetsPaginator(sso, &ssoadmin.ListPermissionSetsInput{InstanceArn: &instance})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list permission sets: %w", err)
		}
		for _, arn := range page.PermissionSets {
			ps, err := sso.DescribePermissionSet(ctx, &ssoadmin.DescribePermissionSetInput{InstanceArn: &instance, PermissionSetArn: &arn})
			if err != nil {
				return nil, nil, fmt.Errorf("read permission set %s: %w", arn, err)
			}
			name := aws.ToString(ps.PermissionSet.Name)
			switch {
			case slices.Contains(servicePermissionSets, name):
				service = append(service, name)
			case strings.HasPrefix(name, "JIT-"):
				o, ok, err := u.orphan(ctx, sso, instance, arn, name)
				if err != nil {
					return nil, nil, err
				}
				if ok {
					orphans = append(orphans, o)
				}
			}
		}
	}
	return service, orphans, nil
}

// orphan reads a JIT-* permission set; ok is false when it lacks the jit=true tag the
// engine puts on every grant, so a permission set someone else named JIT-* is never touched.
func (u *Uninstaller) orphan(ctx context.Context, sso *ssoadmin.Client, instance, arn, name string) (OrphanPS, bool, error) {
	tags, err := sso.ListTagsForResource(ctx, &ssoadmin.ListTagsForResourceInput{InstanceArn: &instance, ResourceArn: &arn})
	if err != nil {
		return OrphanPS{}, false, fmt.Errorf("read the tags of %s: %w", name, err)
	}
	if !slices.ContainsFunc(tags.Tags, func(t ssotypes.Tag) bool { return aws.ToString(t.Key) == "jit" && aws.ToString(t.Value) == "true" }) {
		return OrphanPS{}, false, nil
	}
	o := OrphanPS{ARN: arn, Name: name}
	accounts := ssoadmin.NewListAccountsForProvisionedPermissionSetPaginator(sso,
		&ssoadmin.ListAccountsForProvisionedPermissionSetInput{InstanceArn: &instance, PermissionSetArn: &arn})
	for accounts.HasMorePages() {
		page, err := accounts.NextPage(ctx)
		if err != nil {
			return o, false, fmt.Errorf("list the accounts of %s: %w", name, err)
		}
		for _, account := range page.AccountIds {
			assigned := ssoadmin.NewListAccountAssignmentsPaginator(sso,
				&ssoadmin.ListAccountAssignmentsInput{InstanceArn: &instance, AccountId: &account, PermissionSetArn: &arn})
			for assigned.HasMorePages() {
				ap, err := assigned.NextPage(ctx)
				if err != nil {
					return o, false, fmt.Errorf("list the assignments of %s in %s: %w", name, account, err)
				}
				for _, a := range ap.AccountAssignments {
					o.Assignments = append(o.Assignments, Assignment{Account: account, PrincipalType: string(a.PrincipalType), PrincipalID: aws.ToString(a.PrincipalId)})
				}
			}
		}
	}
	return o, true, nil
}

// DeleteStack starts the deletion of the stack. The retained table and rules stay.
func (u *Uninstaller) DeleteStack(ctx context.Context) error {
	_, err := cloudformation.NewFromConfig(u.cfg).DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: &u.stack})
	if err != nil {
		return fmt.Errorf("delete stack %s: %w", u.stack, err)
	}
	return nil
}

// DeleteTable deletes the grants table; a table already gone is done.
func (u *Uninstaller) DeleteTable(ctx context.Context) error {
	name := u.tableName()
	_, err := dynamodb.NewFromConfig(u.cfg).DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: &name})
	var missing *ddbtypes.ResourceNotFoundException
	if err != nil && !errors.As(err, &missing) {
		return fmt.Errorf("delete table %s: %w", name, err)
	}
	return nil
}

// DeleteRules deletes the rules application: AppConfig deletes an application only
// without profiles, and a profile only without versions. What is already gone is done.
func (u *Uninstaller) DeleteRules(ctx context.Context, appID string) error {
	client := appconfig.NewFromConfig(u.cfg)
	profiles := appconfig.NewListConfigurationProfilesPaginator(client, &appconfig.ListConfigurationProfilesInput{ApplicationId: &appID})
	for profiles.HasMorePages() {
		page, err := profiles.NextPage(ctx)
		if gone(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("list the rules profiles: %w", err)
		}
		for _, p := range page.Items {
			if err := u.deleteProfile(ctx, client, appID, aws.ToString(p.Id)); err != nil {
				return err
			}
		}
	}
	_, err := client.DeleteApplication(ctx, &appconfig.DeleteApplicationInput{ApplicationId: &appID})
	if err != nil && !gone(err) {
		return fmt.Errorf("delete the rules application %s: %w", appID, err)
	}
	return nil
}

func (u *Uninstaller) deleteProfile(ctx context.Context, client *appconfig.Client, appID, profileID string) error {
	versions := appconfig.NewListHostedConfigurationVersionsPaginator(client,
		&appconfig.ListHostedConfigurationVersionsInput{ApplicationId: &appID, ConfigurationProfileId: &profileID})
	for versions.HasMorePages() {
		page, err := versions.NextPage(ctx)
		if gone(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("list the rules versions: %w", err)
		}
		for _, v := range page.Items {
			_, err := client.DeleteHostedConfigurationVersion(ctx, &appconfig.DeleteHostedConfigurationVersionInput{
				ApplicationId: &appID, ConfigurationProfileId: &profileID, VersionNumber: aws.Int32(v.VersionNumber),
			})
			if err != nil && !gone(err) {
				return fmt.Errorf("delete rules version %d: %w", v.VersionNumber, err)
			}
		}
	}
	_, err := client.DeleteConfigurationProfile(ctx, &appconfig.DeleteConfigurationProfileInput{ApplicationId: &appID, ConfigurationProfileId: &profileID})
	if err != nil && !gone(err) {
		return fmt.Errorf("delete the rules profile %s: %w", profileID, err)
	}
	return nil
}

// gone says whether err is a ResourceNotFoundException, of any service.
func gone(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "ResourceNotFoundException"
}

// DeleteOrphan removes the assignments of a JIT-* permission set, waiting for each,
// then the permission set. A permission set already gone is done.
func (u *Uninstaller) DeleteOrphan(ctx context.Context, o OrphanPS) error {
	instance, err := u.instanceARN(ctx)
	if err != nil {
		return err
	}
	sso := ssoadmin.NewFromConfig(u.cfg)
	for _, a := range o.Assignments {
		out, err := sso.DeleteAccountAssignment(ctx, &ssoadmin.DeleteAccountAssignmentInput{
			InstanceArn: &instance, PermissionSetArn: &o.ARN, TargetId: &a.Account, TargetType: ssotypes.TargetTypeAwsAccount,
			PrincipalType: ssotypes.PrincipalType(a.PrincipalType), PrincipalId: &a.PrincipalID,
		})
		if gone(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("remove %s from account %s: %w", o.Name, a.Account, err)
		}
		if err := u.waitDeletion(ctx, sso, instance, aws.ToString(out.AccountAssignmentDeletionStatus.RequestId), o.Name, a.Account); err != nil {
			return err
		}
	}
	_, err = sso.DeletePermissionSet(ctx, &ssoadmin.DeletePermissionSetInput{InstanceArn: &instance, PermissionSetArn: &o.ARN})
	if err != nil && !gone(err) {
		return fmt.Errorf("delete permission set %s (run uninstall again if it is still in use): %w", o.Name, err)
	}
	return nil
}

func (u *Uninstaller) waitDeletion(ctx context.Context, sso *ssoadmin.Client, instance, requestID, name, account string) error {
	for range 120 {
		out, err := sso.DescribeAccountAssignmentDeletionStatus(ctx, &ssoadmin.DescribeAccountAssignmentDeletionStatusInput{
			InstanceArn: &instance, AccountAssignmentDeletionRequestId: &requestID,
		})
		if err != nil {
			return fmt.Errorf("read the removal of %s from account %s: %w", name, account, err)
		}
		switch out.AccountAssignmentDeletionStatus.Status {
		case ssotypes.StatusValuesSucceeded:
			return nil
		case ssotypes.StatusValuesFailed:
			return fmt.Errorf("removing %s from account %s failed: %s", name, account, aws.ToString(out.AccountAssignmentDeletionStatus.FailureReason))
		}
		if err := u.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("removing %s from account %s takes too long: run uninstall again", name, account)
}
