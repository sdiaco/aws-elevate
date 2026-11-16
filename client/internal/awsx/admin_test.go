package awsx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeAdminAWS answers the AppConfig, CloudFormation, Identity Center, CloudTrail and SNS
// calls of the admin commands, recording the AppConfig request.
type fakeAdminAWS struct {
	latest   string // Latest-Version-Number header of the last publication
	content  string
	conflict bool
	noTrail  bool
}

func (f *fakeAdminAWS) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		target := r.Header.Get("X-Amz-Target")
		form := string(body)
		switch {
		case r.URL.Path == "/applications/app1/configurationprofiles/prof1/hostedconfigurationversions":
			if f.conflict {
				w.Header().Set("X-Amzn-Errortype", "ConflictException")
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"Message":"version 4 is not the latest"}`)
				return
			}
			f.latest = r.Header.Get("Latest-Version-Number")
			f.content = string(body)
			w.Header().Set("Version-Number", "5")
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(form, "Action=DescribeStacks"):
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, `<DescribeStacksResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/"><DescribeStacksResult><Stacks><member><StackName>elevate</StackName><StackStatus>UPDATE_COMPLETE</StackStatus><CreationTime>2026-11-15T09:00:00Z</CreationTime></member></Stacks></DescribeStacksResult></DescribeStacksResponse>`)
		case strings.HasSuffix(target, ".ListInstances"):
			fmt.Fprint(w, `{"Instances":[{"InstanceArn":"arn:aws:sso:::instance/ssoins-1"}]}`)
		case strings.HasSuffix(target, ".ListPermissionSets"):
			if strings.Contains(form, `"NextToken"`) {
				fmt.Fprint(w, `{"PermissionSets":["arn:aws:sso:::permissionSet/ssoins-1/ps-2"]}`)
				return
			}
			fmt.Fprint(w, `{"PermissionSets":["arn:aws:sso:::permissionSet/ssoins-1/ps-1"],"NextToken":"more"}`)
		case strings.HasSuffix(target, ".DescribePermissionSet"):
			name := map[bool]string{true: "ElevateAdmin", false: "ElevateRequester"}[strings.Contains(form, "ps-1")]
			fmt.Fprintf(w, `{"PermissionSet":{"Name":%q}}`, name)
		case strings.HasSuffix(target, ".DescribeTrails"):
			if f.noTrail {
				fmt.Fprint(w, `{"trailList":[]}`)
				return
			}
			fmt.Fprint(w, `{"trailList":[{"Name":"org-trail","IsOrganizationTrail":true}]}`)
		case strings.Contains(form, "Action=ListSubscriptionsByTopic"):
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, `<ListSubscriptionsByTopicResponse xmlns="http://sns.amazonaws.com/doc/2010-03-31/"><ListSubscriptionsByTopicResult><Subscriptions>`+
				`<member><SubscriptionArn>arn:aws:sns:eu-west-1:111111111111:elevate-alerts:1</SubscriptionArn></member>`+
				`<member><SubscriptionArn>PendingConfirmation</SubscriptionArn></member>`+
				`</Subscriptions></ListSubscriptionsByTopicResult></ListSubscriptionsByTopicResponse>`)
		default:
			t.Logf("unexpected %s %s target=%q body=%s", r.Method, r.URL, target, form)
			http.Error(w, "unexpected call", http.StatusBadRequest)
		}
	}))
}

func adminFor(srv *httptest.Server) *Admin {
	cfg := aws.Config{Region: "eu-west-1", BaseEndpoint: aws.String(srv.URL)}
	return NewAdmin(cfg, aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"})
}

func TestPublishRulesStartsFromTheGivenVersion(t *testing.T) {
	f := &fakeAdminAWS{}
	srv := f.serve(t)
	defer srv.Close()
	latest := 4
	n, err := adminFor(srv).PublishRules(context.Background(), "app1", "prof1", &latest, []byte(`{"rules":[]}`))
	if err != nil || n != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if f.latest != "4" || f.content != `{"rules":[]}` {
		t.Fatalf("latest %q content %q", f.latest, f.content)
	}
	if _, err := adminFor(srv).PublishRules(context.Background(), "app1", "prof1", nil, []byte(`{}`)); err != nil || f.latest != "" {
		t.Fatalf("first version: latest %q err %v", f.latest, err)
	}
}

func TestPublishRulesConflictIsErrVersionConflict(t *testing.T) {
	f := &fakeAdminAWS{conflict: true}
	srv := f.serve(t)
	defer srv.Close()
	latest := 3
	if _, err := adminFor(srv).PublishRules(context.Background(), "app1", "prof1", &latest, []byte(`{}`)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("err %v", err)
	}
}

func TestDoctorReadsWithTheSDK(t *testing.T) {
	f := &fakeAdminAWS{}
	srv := f.serve(t)
	defer srv.Close()
	a := adminFor(srv)
	ctx := context.Background()
	if status, err := a.StackStatus(ctx, "arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1"); err != nil || status != "UPDATE_COMPLETE" {
		t.Errorf("stack %q %v", status, err)
	}
	if names, err := a.PermissionSetNames(ctx); err != nil || strings.Join(names, ",") != "ElevateAdmin,ElevateRequester" {
		t.Errorf("permission sets %v %v", names, err)
	}
	if has, err := a.HasTrail(ctx); err != nil || !has {
		t.Errorf("trail %v %v", has, err)
	}
	f.noTrail = true
	if has, err := a.HasTrail(ctx); err != nil || has {
		t.Errorf("no trail %v %v", has, err)
	}
	if n, err := a.ConfirmedSubscribers(ctx, "arn:aws:sns:eu-west-1:111111111111:elevate-alerts"); err != nil || n != 1 {
		t.Errorf("subscribers %d %v", n, err)
	}
}

// fakePreview answers DescribeStacks, CreateChangeSet and DescribeChangeSet: the change
// set is in progress at the first read, then complete over two pages, or failed.
type fakePreview struct {
	failReason string
	create     url.Values
	reads      int
}

func (f *fakePreview) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "text/xml")
		change := func(action, id string) string {
			return `<member><Type>Resource</Type><ResourceChange><Action>` + action + `</Action><LogicalResourceId>` + id +
				`</LogicalResourceId><ResourceType>AWS::Lambda::Function</ResourceType><Replacement>False</Replacement></ResourceChange></member>`
		}
		switch form.Get("Action") {
		case "DescribeStacks":
			fmt.Fprint(w, `<DescribeStacksResponse><DescribeStacksResult><Stacks><member><StackName>elevate</StackName><StackStatus>CREATE_COMPLETE</StackStatus><CreationTime>2026-11-16T09:00:00Z</CreationTime><Parameters>`+
				`<member><ParameterKey>AdminGroupId</ParameterKey><ParameterValue>g-1</ParameterValue></member>`+
				`<member><ParameterKey>CodeKey</ParameterKey><ParameterValue>v0.0.1/lambda.zip</ParameterValue></member>`+
				`</Parameters></member></Stacks></DescribeStacksResult></DescribeStacksResponse>`)
		case "CreateChangeSet":
			f.create = form
			fmt.Fprint(w, `<CreateChangeSetResponse><CreateChangeSetResult><Id>cs-1</Id></CreateChangeSetResult></CreateChangeSetResponse>`)
		case "DescribeChangeSet":
			f.reads++
			switch {
			case f.reads == 1:
				fmt.Fprint(w, `<DescribeChangeSetResponse><DescribeChangeSetResult><Status>CREATE_IN_PROGRESS</Status></DescribeChangeSetResult></DescribeChangeSetResponse>`)
			case f.failReason != "":
				fmt.Fprintf(w, `<DescribeChangeSetResponse><DescribeChangeSetResult><Status>FAILED</Status><StatusReason>%s</StatusReason></DescribeChangeSetResult></DescribeChangeSetResponse>`, f.failReason)
			case form.Get("NextToken") == "":
				fmt.Fprint(w, `<DescribeChangeSetResponse><DescribeChangeSetResult><Status>CREATE_COMPLETE</Status><Changes>`+change("Modify", "EngineFunction")+`</Changes><NextToken>p2</NextToken></DescribeChangeSetResult></DescribeChangeSetResponse>`)
			default:
				fmt.Fprint(w, `<DescribeChangeSetResponse><DescribeChangeSetResult><Status>CREATE_COMPLETE</Status><Changes>`+change("Add", "ApiFunction")+`</Changes></DescribeChangeSetResult></DescribeChangeSetResponse>`)
			}
		default:
			t.Logf("unexpected %s", body)
			http.Error(w, "unexpected call", http.StatusBadRequest)
		}
	}))
}

func previewAdmin(srv *httptest.Server) *Admin {
	a := adminFor(srv)
	a.sleep = func(context.Context, time.Duration) error { return nil }
	a.now = func() time.Time { return time.Unix(1700000000, 0) }
	return a
}

func TestPreviewUpdateKeepsParametersButCodeKeyAndReadsEveryChange(t *testing.T) {
	f := &fakePreview{}
	srv := f.serve(t)
	defer srv.Close()
	p, err := previewAdmin(srv).PreviewUpdate(context.Background(), "stack-1", "https://example.com/v0.2.0/elevate.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Parameters, ",") != "AdminGroupId" || p.NoChanges || len(p.Changes) != 2 {
		t.Fatalf("%+v", p)
	}
	if p.Changes[1] != (Change{Action: "Add", Resource: "ApiFunction", Type: "AWS::Lambda::Function", Replacement: "False"}) {
		t.Errorf("changes %+v", p.Changes)
	}
	c := f.create
	if c.Get("ChangeSetType") != "UPDATE" || c.Get("ChangeSetName") != "elevate-preview-1700000000" ||
		c.Get("TemplateURL") != "https://example.com/v0.2.0/elevate.yaml" || c.Get("Capabilities.member.1") != "CAPABILITY_NAMED_IAM" ||
		c.Get("Parameters.member.1.UsePreviousValue") != "true" || c.Get("Parameters.member.2.ParameterKey") != "" {
		t.Errorf("request %v", c)
	}
}

func TestPreviewUpdateWithoutChangesOrFailed(t *testing.T) {
	f := &fakePreview{failReason: "The submitted information didn't contain changes. Submit different information to create a change set."}
	srv := f.serve(t)
	p, err := previewAdmin(srv).PreviewUpdate(context.Background(), "stack-1", "https://example.com/t.yaml")
	srv.Close()
	if err != nil || !p.NoChanges {
		t.Errorf("no changes: %+v %v", p, err)
	}
	f = &fakePreview{failReason: "S3 error: Access Denied"}
	srv = f.serve(t)
	defer srv.Close()
	if _, err := previewAdmin(srv).PreviewUpdate(context.Background(), "stack-1", "https://example.com/t.yaml"); err == nil || !strings.Contains(err.Error(), "Access Denied") {
		t.Errorf("failed: %v", err)
	}
}
