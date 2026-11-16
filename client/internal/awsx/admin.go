package awsx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfig"
	appconfigtypes "github.com/aws/aws-sdk-go-v2/service/appconfig/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/ssoadmin"
)

// ErrVersionConflict: another rules version was published after the one the save
// started from (design §7.1).
var ErrVersionConflict = errors.New("another rules version was published meanwhile")

// Admin makes the AWS calls of the admin commands with ElevateAdmin credentials: the
// publication of rules (design §7.2) and the reads of doctor.
type Admin struct {
	cfg   aws.Config
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

// NewAdmin signs every call with creds.
func NewAdmin(cfg aws.Config, creds aws.Credentials) *Admin {
	return &Admin{cfg: WithCredentials(cfg, creds), sleep: Sleep, now: time.Now}
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

// Preview is a change set of the stack: what an update would change.
type Preview struct {
	// Parameters keep their current value; the update command must pass the same.
	Parameters []string
	Changes    []Change
	NoChanges  bool
}

type Change struct {
	Action      string
	Resource    string
	Type        string
	Replacement string
}

// previewPoll is how often PreviewUpdate reads the change set; it takes about 8 s
// (spike C10).
const previewPoll = 2 * time.Second

// PreviewUpdate creates a change set of the stack from templateURL and waits for it: it
// changes nothing (design §12.3). Every parameter keeps its value except CodeKey, whose
// default names the Lambda package of the new release.
func (a *Admin) PreviewUpdate(ctx context.Context, stackID, templateURL string) (Preview, error) {
	client := cloudformation.NewFromConfig(a.cfg)
	stacks, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &stackID})
	if err != nil {
		return Preview{}, fmt.Errorf("read stack: %w", err)
	}
	if len(stacks.Stacks) == 0 {
		return Preview{}, fmt.Errorf("stack %s not found", stackID)
	}
	var p Preview
	in := &cloudformation.CreateChangeSetInput{
		StackName:     &stackID,
		ChangeSetName: aws.String(fmt.Sprintf("elevate-preview-%d", a.now().Unix())),
		ChangeSetType: cfntypes.ChangeSetTypeUpdate,
		TemplateURL:   &templateURL,
		Capabilities:  []cfntypes.Capability{cfntypes.CapabilityCapabilityNamedIam},
	}
	for _, par := range stacks.Stacks[0].Parameters {
		key := aws.ToString(par.ParameterKey)
		if key == "CodeKey" {
			continue
		}
		p.Parameters = append(p.Parameters, key)
		in.Parameters = append(in.Parameters, cfntypes.Parameter{ParameterKey: aws.String(key), UsePreviousValue: aws.Bool(true)})
	}
	created, err := client.CreateChangeSet(ctx, in)
	if err != nil {
		return Preview{}, fmt.Errorf("create the preview: %w", err)
	}
	for {
		d, err := client.DescribeChangeSet(ctx, &cloudformation.DescribeChangeSetInput{ChangeSetName: created.Id, StackName: &stackID})
		if err != nil {
			return Preview{}, fmt.Errorf("read the preview: %w", err)
		}
		switch d.Status {
		case cfntypes.ChangeSetStatusFailed:
			// CloudFormation fails a change set that changes nothing, with this reason.
			if strings.Contains(aws.ToString(d.StatusReason), "didn't contain changes") {
				p.NoChanges = true
				return p, nil
			}
			return Preview{}, fmt.Errorf("preview failed: %s", aws.ToString(d.StatusReason))
		case cfntypes.ChangeSetStatusCreateComplete:
			for {
				for _, c := range d.Changes {
					rc := c.ResourceChange
					if rc == nil {
						continue
					}
					p.Changes = append(p.Changes, Change{
						Action:      string(rc.Action),
						Resource:    aws.ToString(rc.LogicalResourceId),
						Type:        aws.ToString(rc.ResourceType),
						Replacement: string(rc.Replacement),
					})
				}
				if d.NextToken == nil {
					return p, nil
				}
				d, err = client.DescribeChangeSet(ctx, &cloudformation.DescribeChangeSetInput{ChangeSetName: created.Id, StackName: &stackID, NextToken: d.NextToken})
				if err != nil {
					return Preview{}, fmt.Errorf("read the preview: %w", err)
				}
			}
		}
		if err := a.sleep(ctx, previewPoll); err != nil {
			return Preview{}, err
		}
	}
}
