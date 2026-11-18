package awsx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeInstallAWS answers the STS, Identity Center, Organizations, Identity Store and
// CloudFormation calls of the guided install.
type fakeInstallAWS struct {
	noInstance  bool
	management  bool
	notDelegate bool
	exists      bool
	create      url.Values // the CreateStack request
}

func (f *fakeInstallAWS) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		target := r.Header.Get("X-Amz-Target")
		form, _ := url.ParseQuery(string(body))
		xml := func(s string) {
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, s)
		}
		switch {
		case form.Get("Action") == "GetCallerIdentity":
			xml(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Account>111111111111</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
		case strings.HasSuffix(target, ".ListInstances"):
			if f.noInstance {
				fmt.Fprint(w, `{"Instances":[]}`)
				return
			}
			fmt.Fprint(w, `{"Instances":[{"InstanceArn":"arn:aws:sso:::instance/ssoins-1","IdentityStoreId":"d-1"}]}`)
		case strings.HasSuffix(target, ".DescribeOrganization"):
			master := map[bool]string{true: "111111111111", false: "222222222222"}[f.management]
			fmt.Fprintf(w, `{"Organization":{"MasterAccountId":%q}}`, master)
		case strings.HasSuffix(target, ".ListPermissionSets"):
			if f.notDelegate {
				w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"Message":"not authorized"}`)
				return
			}
			fmt.Fprint(w, `{"PermissionSets":[]}`)
		case strings.HasSuffix(target, ".ListGroups"):
			if strings.Contains(string(body), `"NextToken"`) {
				fmt.Fprint(w, `{"Groups":[{"GroupId":"g-2","DisplayName":"devs"}]}`)
				return
			}
			fmt.Fprint(w, `{"Groups":[{"GroupId":"g-1","DisplayName":"admins"}],"NextToken":"more"}`)
		case form.Get("Action") == "CreateStack":
			if f.exists {
				w.Header().Set("Content-Type", "text/xml")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `<ErrorResponse><Error><Type>Sender</Type><Code>AlreadyExistsException</Code><Message>Stack [elevate] already exists</Message></Error></ErrorResponse>`)
				return
			}
			f.create = form
			xml(`<CreateStackResponse><CreateStackResult><StackId>arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1</StackId></CreateStackResult></CreateStackResponse>`)
		case form.Get("Action") == "DescribeStacks":
			xml(`<DescribeStacksResponse><DescribeStacksResult><Stacks><member><StackName>elevate</StackName><StackStatus>CREATE_IN_PROGRESS</StackStatus><CreationTime>2026-11-16T09:00:00Z</CreationTime></member></Stacks></DescribeStacksResult></DescribeStacksResponse>`)
		case form.Get("Action") == "DescribeStackEvents":
			// newest first, as CloudFormation returns them
			xml(`<DescribeStackEventsResponse><DescribeStackEventsResult><StackEvents>` +
				`<member><EventId>e2</EventId><StackId>s</StackId><StackName>elevate</StackName><Timestamp>2026-11-16T09:00:05Z</Timestamp><LogicalResourceId>Precheck</LogicalResourceId><ResourceStatus>CREATE_FAILED</ResourceStatus><ResourceStatusReason>wrong account</ResourceStatusReason></member>` +
				`<member><EventId>e1</EventId><StackId>s</StackId><StackName>elevate</StackName><Timestamp>2026-11-16T09:00:00Z</Timestamp><LogicalResourceId>elevate</LogicalResourceId><ResourceStatus>CREATE_IN_PROGRESS</ResourceStatus></member>` +
				`</StackEvents></DescribeStackEventsResult></DescribeStackEventsResponse>`)
		default:
			t.Logf("unexpected %s %s target=%q body=%s", r.Method, r.URL, target, body)
			http.Error(w, "unexpected call", http.StatusBadRequest)
		}
	}))
}

func installerFor(srv *httptest.Server) *Installer {
	return &Installer{cfg: aws.Config{
		Region:       "eu-west-1",
		BaseEndpoint: aws.String(srv.URL),
		Credentials:  static(aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}),
	}}
}

func TestInstallCheckFindsTheIdentityStore(t *testing.T) {
	srv := (&fakeInstallAWS{}).serve(t)
	defer srv.Close()
	got, err := installerFor(srv).Check(context.Background())
	if err != nil || got != (Installation{Account: "111111111111", IdentityStoreID: "d-1"}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestInstallCheckRefusesWrongRegionAndAccount(t *testing.T) {
	cases := map[string]*fakeInstallAWS{
		"no IAM Identity Center instance visible in eu-west-1":                    {noInstance: true},
		"account 111111111111 is the management account":                          {management: true},
		"account 111111111111 is not the Identity Center delegated administrator": {notDelegate: true},
	}
	for want, f := range cases {
		srv := f.serve(t)
		_, err := installerFor(srv).Check(context.Background())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err %v, want %q", err, want)
		}
	}
}

func TestInstallCheckSaysHowToRegisterTheDelegatedAdministrator(t *testing.T) {
	register := "aws organizations register-delegated-administrator --account-id 111111111111 --service-principal sso.amazonaws.com"
	for _, f := range []*fakeInstallAWS{{noInstance: true}, {notDelegate: true}} {
		srv := f.serve(t)
		_, err := installerFor(srv).Check(context.Background())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), register) {
			t.Errorf("err %v", err)
		}
	}
}

func TestInstallListsEveryGroup(t *testing.T) {
	srv := (&fakeInstallAWS{}).serve(t)
	defer srv.Close()
	groups, err := installerFor(srv).Groups(context.Background(), "d-1")
	if err != nil || len(groups) != 2 || groups[1] != (Group{ID: "g-2", Name: "devs"}) {
		t.Fatalf("%v %v", groups, err)
	}
}

func TestInstallCreatesTheStackDeletedOnFailure(t *testing.T) {
	f := &fakeInstallAWS{}
	srv := f.serve(t)
	defer srv.Close()
	id, err := installerFor(srv).CreateStack(context.Background(), NewStack{
		Name: "elevate", TemplateURL: "https://example.com/elevate.yaml",
		Parameters: map[string]string{"RequesterGroupId": "g-2", "AdminGroupId": "g-1"},
	})
	if err != nil || !strings.HasSuffix(id, "stack/elevate/1") {
		t.Fatalf("%q %v", id, err)
	}
	c := f.create
	if c.Get("OnFailure") != "DELETE" || c.Get("Capabilities.member.1") != "CAPABILITY_NAMED_IAM" ||
		c.Get("Parameters.member.1.ParameterKey") != "AdminGroupId" || c.Get("Parameters.member.1.ParameterValue") != "g-1" {
		t.Errorf("request %v", c)
	}
}

func TestInstallSaysTheStackAlreadyExists(t *testing.T) {
	srv := (&fakeInstallAWS{exists: true}).serve(t)
	defer srv.Close()
	_, err := installerFor(srv).CreateStack(context.Background(), NewStack{Name: "elevate", TemplateURL: "https://example.com/elevate.yaml"})
	if err == nil || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "admin updates") {
		t.Fatalf("err %v", err)
	}
}

func TestInstallReadsTheStackEventsOldestFirst(t *testing.T) {
	srv := (&fakeInstallAWS{}).serve(t)
	defer srv.Close()
	s, err := installerFor(srv).Stack(context.Background(), "arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1")
	if err != nil || s.Status != "CREATE_IN_PROGRESS" || len(s.Events) != 2 {
		t.Fatalf("%+v %v", s, err)
	}
	if s.Events[0].ID != "e1" || s.Events[1] != (StackEvent{ID: "e2", Resource: "Precheck", Status: "CREATE_FAILED", Reason: "wrong account"}) {
		t.Errorf("events %+v", s.Events)
	}
}
