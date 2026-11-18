package awsx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeUninstallAWS answers the CloudFormation, DynamoDB, AppConfig and Identity Center
// calls of the uninstall, from what is left; calls records the deletions in order.
type fakeUninstallAWS struct {
	stack     bool
	table     bool
	app       bool
	versions  []int
	permSets  map[string]string // arn → name
	untagged  string            // arn of a JIT-* without the jit tag
	assigned  map[string]bool   // arn → assigned to user u-1 in 222222222222
	calls     []string
	deleteErr string // error code answered to DeletePermissionSet
}

func (f *fakeUninstallAWS) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		target := r.Header.Get("X-Amz-Target")
		form, _ := url.ParseQuery(string(body))
		xml := func(s string) {
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, s)
		}
		jsonErr := func(status int, code string) {
			w.Header().Set("X-Amzn-Errortype", code)
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"__type":%q,"message":"not found","Message":"not found"}`, code)
		}
		op := target[strings.LastIndex(target, ".")+1:]
		path := r.URL.Path
		switch {
		case form.Get("Action") == "DescribeStacks":
			if !f.stack {
				w.Header().Set("Content-Type", "text/xml")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `<ErrorResponse><Error><Type>Sender</Type><Code>ValidationError</Code><Message>Stack with id elevate does not exist</Message></Error></ErrorResponse>`)
				return
			}
			xml(`<DescribeStacksResponse><DescribeStacksResult><Stacks><member><StackId>arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1</StackId><StackName>elevate</StackName><StackStatus>UPDATE_COMPLETE</StackStatus><CreationTime>2026-11-16T09:00:00Z</CreationTime></member></Stacks></DescribeStacksResult></DescribeStacksResponse>`)
		case form.Get("Action") == "ListStackResources":
			if form.Get("NextToken") == "" {
				xml(`<ListStackResourcesResponse><ListStackResourcesResult><NextToken>more</NextToken><StackResourceSummaries><member><LogicalResourceId>A</LogicalResourceId></member><member><LogicalResourceId>B</LogicalResourceId></member></StackResourceSummaries></ListStackResourcesResult></ListStackResourcesResponse>`)
				return
			}
			xml(`<ListStackResourcesResponse><ListStackResourcesResult><StackResourceSummaries><member><LogicalResourceId>C</LogicalResourceId></member></StackResourceSummaries></ListStackResourcesResult></ListStackResourcesResponse>`)
		case form.Get("Action") == "DeleteStack":
			f.calls = append(f.calls, "stack "+form.Get("StackName"))
			xml(`<DeleteStackResponse><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></DeleteStackResponse>`)
		case op == "DescribeTable" || op == "DeleteTable":
			if !strings.Contains(string(body), `"elevate-grants"`) {
				t.Errorf("table %s", body)
			}
			if !f.table {
				jsonErr(http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException")
				return
			}
			if op == "DeleteTable" {
				f.calls = append(f.calls, "table")
				f.table = false
			}
			fmt.Fprint(w, `{"Table":{"TableName":"elevate-grants"}}`)
		case r.Method == http.MethodGet && path == "/applications":
			if f.app {
				fmt.Fprint(w, `{"Items":[{"Id":"other","Name":"elevate-e2e"},{"Id":"app1","Name":"elevate"}]}`)
				return
			}
			fmt.Fprint(w, `{"Items":[{"Id":"other","Name":"elevate-e2e"}]}`)
		case r.Method == http.MethodGet && path == "/applications/app1/configurationprofiles":
			fmt.Fprint(w, `{"Items":[{"Id":"prof1"}]}`)
		case r.Method == http.MethodGet && path == "/applications/app1/configurationprofiles/prof1/hostedconfigurationversions":
			items := make([]string, len(f.versions))
			for i, v := range f.versions {
				items[i] = fmt.Sprintf(`{"VersionNumber":%d}`, v)
			}
			fmt.Fprintf(w, `{"Items":[%s]}`, strings.Join(items, ","))
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/applications/app1"):
			f.calls = append(f.calls, "appconfig "+strings.TrimPrefix(path, "/applications/"))
			if path == "/applications/app1/configurationprofiles/prof1/hostedconfigurationversions/2" {
				jsonErr(http.StatusNotFound, "ResourceNotFoundException") // deleted by an earlier run
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case op == "ListInstances":
			fmt.Fprint(w, `{"Instances":[{"InstanceArn":"arn:aws:sso:::instance/ssoins-1","IdentityStoreId":"d-1"}]}`)
		case op == "ListPermissionSets":
			arns := make([]string, 0, len(f.permSets))
			for arn := range f.permSets {
				arns = append(arns, fmt.Sprintf("%q", arn))
			}
			fmt.Fprintf(w, `{"PermissionSets":[%s]}`, strings.Join(arns, ","))
		case op == "DescribePermissionSet":
			for arn, name := range f.permSets {
				if strings.Contains(string(body), arn) {
					fmt.Fprintf(w, `{"PermissionSet":{"PermissionSetArn":%q,"Name":%q}}`, arn, name)
					return
				}
			}
			t.Errorf("describe %s", body)
		case op == "ListTagsForResource":
			if f.untagged != "" && strings.Contains(string(body), f.untagged) {
				fmt.Fprint(w, `{"Tags":[]}`)
				return
			}
			fmt.Fprint(w, `{"Tags":[{"Key":"jit:grantId","Value":"x"},{"Key":"jit","Value":"true"}]}`)
		case op == "ListAccountsForProvisionedPermissionSet":
			for arn, yes := range f.assigned {
				if yes && strings.Contains(string(body), arn) {
					fmt.Fprint(w, `{"AccountIds":["222222222222"]}`)
					return
				}
			}
			fmt.Fprint(w, `{"AccountIds":[]}`)
		case op == "ListAccountAssignments":
			fmt.Fprint(w, `{"AccountAssignments":[{"AccountId":"222222222222","PrincipalType":"USER","PrincipalId":"u-1"}]}`)
		case op == "DeleteAccountAssignment":
			f.calls = append(f.calls, "assignment")
			fmt.Fprint(w, `{"AccountAssignmentDeletionStatus":{"RequestId":"req-1","Status":"IN_PROGRESS"}}`)
		case op == "DescribeAccountAssignmentDeletionStatus":
			f.calls = append(f.calls, "wait")
			status := "IN_PROGRESS"
			if strings.Count(strings.Join(f.calls, ","), "wait") > 1 {
				status = "SUCCEEDED"
			}
			fmt.Fprintf(w, `{"AccountAssignmentDeletionStatus":{"RequestId":"req-1","Status":%q}}`, status)
		case op == "DeletePermissionSet":
			f.calls = append(f.calls, "permission set")
			if f.deleteErr != "" {
				jsonErr(http.StatusBadRequest, f.deleteErr)
				return
			}
			fmt.Fprint(w, `{}`)
		default:
			t.Logf("unexpected %s %s target=%q body=%s", r.Method, r.URL, target, body)
			http.Error(w, "unexpected call", http.StatusBadRequest)
		}
	}))
}

func uninstallerFor(srv *httptest.Server) *Uninstaller {
	return &Uninstaller{Installer: installerFor(srv), stack: "elevate", sleep: func(context.Context, time.Duration) error { return nil }}
}

func TestLeftoversFindsEveryKindOfResource(t *testing.T) {
	f := &fakeUninstallAWS{stack: true, table: true, app: true,
		permSets: map[string]string{
			"arn:ps-req": "ElevateRequester", "arn:ps-jit": "JIT-abc", "arn:ps-own": "JIT-not-ours", "arn:ps-other": "ReadOnly",
		},
		untagged: "arn:ps-own",
		assigned: map[string]bool{"arn:ps-jit": true},
	}
	srv := f.serve(t)
	defer srv.Close()
	got, err := uninstallerFor(srv).Leftovers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Leftovers{
		Stack:     &StackInfo{ID: "arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1", Status: "UPDATE_COMPLETE", Resources: 3},
		Table:     true,
		RulesApp:  "app1",
		ServicePS: []string{"ElevateRequester"},
		Orphans: []OrphanPS{{ARN: "arn:ps-jit", Name: "JIT-abc",
			Assignments: []Assignment{{Account: "222222222222", PrincipalType: "USER", PrincipalID: "u-1"}}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if got.Empty() {
		t.Error("empty")
	}
}

func TestLeftoversWhenNothingIsLeft(t *testing.T) {
	srv := (&fakeUninstallAWS{}).serve(t)
	defer srv.Close()
	got, err := uninstallerFor(srv).Leftovers(context.Background())
	if err != nil || !got.Empty() {
		t.Errorf("%+v %v", got, err)
	}
}

func TestDeleteTableToleratesNotFound(t *testing.T) {
	f := &fakeUninstallAWS{table: true}
	srv := f.serve(t)
	defer srv.Close()
	u := uninstallerFor(srv)
	for range 2 {
		if err := u.DeleteTable(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(f.calls, ",") != "table" {
		t.Errorf("calls %v", f.calls)
	}
}

func TestDeleteRulesDeletesVersionsBeforeTheApplication(t *testing.T) {
	f := &fakeUninstallAWS{app: true, versions: []int{1, 2}}
	srv := f.serve(t)
	defer srv.Close()
	if err := uninstallerFor(srv).DeleteRules(context.Background(), "app1"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"appconfig app1/configurationprofiles/prof1/hostedconfigurationversions/1",
		"appconfig app1/configurationprofiles/prof1/hostedconfigurationversions/2",
		"appconfig app1/configurationprofiles/prof1",
		"appconfig app1",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls %v", f.calls)
	}
}

func TestDeleteOrphanRemovesAssignmentsFirst(t *testing.T) {
	f := &fakeUninstallAWS{}
	srv := f.serve(t)
	defer srv.Close()
	o := OrphanPS{ARN: "arn:ps-jit", Name: "JIT-abc", Assignments: []Assignment{{Account: "222222222222", PrincipalType: "USER", PrincipalID: "u-1"}}}
	if err := uninstallerFor(srv).DeleteOrphan(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, ",") != "assignment,wait,wait,permission set" {
		t.Errorf("calls %v", f.calls)
	}
}

func TestDeleteOrphanSaysToRunAgainWhenStillInUse(t *testing.T) {
	f := &fakeUninstallAWS{deleteErr: "ConflictException"}
	srv := f.serve(t)
	defer srv.Close()
	err := uninstallerFor(srv).DeleteOrphan(context.Background(), OrphanPS{ARN: "arn:ps-jit", Name: "JIT-abc"})
	if err == nil || !strings.Contains(err.Error(), "run uninstall again") {
		t.Errorf("err %v", err)
	}
}

func TestDeleteStack(t *testing.T) {
	f := &fakeUninstallAWS{stack: true}
	srv := f.serve(t)
	defer srv.Close()
	if err := uninstallerFor(srv).DeleteStack(context.Background()); err != nil || strings.Join(f.calls, ",") != "stack elevate" {
		t.Errorf("%v %v", err, f.calls)
	}
}
