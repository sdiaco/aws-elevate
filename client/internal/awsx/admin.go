package awsx

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfig"
	appconfigtypes "github.com/aws/aws-sdk-go-v2/service/appconfig/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/ssoadmin"
)

// ErrVersionConflict: another rules version was published after the one the save
// started from (design §7.1).
var ErrVersionConflict = errors.New("another rules version was published meanwhile")

// Admin makes the AWS calls of the admin commands with ElevateAdmin credentials: the
// publication of rules (design §7.2) and the reads of doctor.
type Admin struct{ cfg aws.Config }

// NewAdmin signs every call with creds.
func NewAdmin(cfg aws.Config, creds aws.Credentials) *Admin {
	return &Admin{cfg: WithCredentials(cfg, creds)}
}

// PublishRules creates a hosted configuration version. latest is the newest version
// number the save started from (nil when there is none yet); AppConfig refuses the
// version if a newer one exists. It returns the number of the version that is now the
// latest: the same as latest when the content did not change (spike C4).
func (a *Admin) PublishRules(ctx context.Context, app, profile string, latest *int, doc []byte) (int, error) {
	in := &appconfig.CreateHostedConfigurationVersionInput{
		ApplicationId:          &app,
		ConfigurationProfileId: &profile,
		Content:                doc,
		ContentType:            aws.String("application/json"),
	}
	if latest != nil {
		in.LatestVersionNumber = aws.Int32(int32(*latest))
	}
	out, err := appconfig.NewFromConfig(a.cfg).CreateHostedConfigurationVersion(ctx, in)
	var conflict *appconfigtypes.ConflictException
	if errors.As(err, &conflict) {
		return 0, ErrVersionConflict
	}
	if err != nil {
		return 0, err
	}
	return int(out.VersionNumber), nil
}

// StackStatus is the CloudFormation status of the stack, for example UPDATE_COMPLETE.
func (a *Admin) StackStatus(ctx context.Context, stackID string) (string, error) {
	out, err := cloudformation.NewFromConfig(a.cfg).DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &stackID})
	if err != nil {
		return "", err
	}
	if len(out.Stacks) == 0 {
		return "", fmt.Errorf("stack %s not found", stackID)
	}
	return string(out.Stacks[0].StackStatus), nil
}

// PermissionSetNames lists the permission sets of the Identity Center instance.
func (a *Admin) PermissionSetNames(ctx context.Context) ([]string, error) {
	client := ssoadmin.NewFromConfig(a.cfg)
	inst, err := client.ListInstances(ctx, &ssoadmin.ListInstancesInput{})
	if err != nil {
		return nil, err
	}
	if len(inst.Instances) == 0 {
		return nil, errors.New("no IAM Identity Center instance visible from this account")
	}
	arn := inst.Instances[0].InstanceArn
	var names []string
	pages := ssoadmin.NewListPermissionSetsPaginator(client, &ssoadmin.ListPermissionSetsInput{InstanceArn: arn})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, ps := range page.PermissionSets {
			d, err := client.DescribePermissionSet(ctx, &ssoadmin.DescribePermissionSetInput{InstanceArn: arn, PermissionSetArn: aws.String(ps)})
			if err != nil {
				return nil, err
			}
			names = append(names, aws.ToString(d.PermissionSet.Name))
		}
	}
	return names, nil
}

// HasTrail tells whether any CloudTrail trail, an organization trail included, covers
// this account and region.
func (a *Admin) HasTrail(ctx context.Context) (bool, error) {
	out, err := cloudtrail.NewFromConfig(a.cfg).DescribeTrails(ctx, &cloudtrail.DescribeTrailsInput{IncludeShadowTrails: aws.Bool(true)})
	if err != nil {
		return false, err
	}
	return len(out.TrailList) > 0, nil
}

// ConfirmedSubscribers counts the subscriptions of a topic that are confirmed.
func (a *Admin) ConfirmedSubscribers(ctx context.Context, topicARN string) (int, error) {
	n := 0
	pages := sns.NewListSubscriptionsByTopicPaginator(sns.NewFromConfig(a.cfg), &sns.ListSubscriptionsByTopicInput{TopicArn: &topicARN})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		for _, s := range page.Subscriptions {
			// SNS shows an unconfirmed subscription with this placeholder instead of an ARN.
			if aws.ToString(s.SubscriptionArn) != "PendingConfirmation" {
				n++
			}
		}
	}
	return n, nil
}
