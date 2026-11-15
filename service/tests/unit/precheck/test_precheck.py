import json

import pytest
from botocore.exceptions import ClientError

from aws_elevate.precheck import handler as precheck

EVENT = {
    "RequestType": "Create",
    "ResponseURL": "https://cloudformation-custom-resource-response.example/abc",
    "StackId": "arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1",
    "RequestId": "req-1",
    "LogicalResourceId": "Precheck",
}
INSTANCE = {
    "InstanceArn": "arn:aws:sso:::instance/ssoins-1111111111111111",
    "IdentityStoreId": "d-1111111111",
}


class FakeSso:
    def __init__(self, instances=(INSTANCE,), list_error=None):
        self.instances = list(instances)
        self.list_error = list_error

    def list_instances(self):
        return {"Instances": self.instances}

    def list_permission_sets(self, InstanceArn, MaxResults):
        if self.list_error:
            raise self.list_error
        return {"PermissionSets": []}


class FakeOrg:
    def __init__(self, management="222222222222"):
        self.management = management

    def describe_organization(self):
        return {"Organization": {"MasterAccountId": self.management}}


class Sent(list):
    def __call__(self, url, body):
        self.append((url, json.loads(body)))


def run(event=EVENT, sso=None, org=None):
    sent = Sent()
    precheck.respond_to(
        event, sso=sso or FakeSso(), org=org or FakeOrg(), account="111111111111", send=sent
    )
    assert len(sent) == 1, "exactly one answer to CloudFormation"
    url, body = sent[0]
    assert url == EVENT["ResponseURL"]
    for key in ("StackId", "RequestId", "LogicalResourceId"):
        assert body[key] == EVENT[key]
    return body


def test_success_returns_instance_data():
    body = run()
    assert body["Status"] == "SUCCESS"
    assert body["PhysicalResourceId"] == "elevate-precheck"
    assert body["Data"] == {
        "InstanceArn": INSTANCE["InstanceArn"],
        "InstanceId": "ssoins-1111111111111111",
        "IdentityStoreId": "d-1111111111",
    }


def test_wrong_region():
    body = run(sso=FakeSso(instances=()))
    assert body["Status"] == "FAILED"
    assert "home region" in body["Reason"]


def test_account_not_delegated_admin():
    denied = ClientError({"Error": {"Code": "AccessDeniedException", "Message": "no"}}, "List")
    body = run(sso=FakeSso(list_error=denied))
    assert body["Status"] == "FAILED"
    assert "delegated administrator" in body["Reason"]


def test_management_account_is_refused():
    body = run(org=FakeOrg(management="111111111111"))
    assert body["Status"] == "FAILED"
    assert "management account" in body["Reason"]


def test_internal_error_still_answers():
    class Broken(FakeSso):
        def list_instances(self):
            raise KeyError("boom")

    body = run(sso=Broken())
    assert body["Status"] == "FAILED"
    assert "KeyError" in body["Reason"]


@pytest.mark.parametrize("request_type", ["Delete", "Update"])
def test_delete_and_update_skip_checks_on_delete(request_type):
    body = run(event={**EVENT, "RequestType": request_type}, sso=FakeSso(instances=()))
    if request_type == "Delete":
        assert body["Status"] == "SUCCESS"
    else:
        assert body["Status"] == "FAILED"


def test_reason_is_short_enough_for_cloudformation():
    class Long(FakeSso):
        def list_instances(self):
            raise RuntimeError("x" * 10000)

    assert len(run(sso=Long())["Reason"]) <= 1000


def test_handler_builds_clients_and_sends(monkeypatch):
    calls = []
    monkeypatch.setattr(precheck, "respond_to", lambda event, **kw: calls.append((event, kw)))
    monkeypatch.setenv("AWS_DEFAULT_REGION", "eu-west-1")

    class Ctx:
        invoked_function_arn = "arn:aws:lambda:eu-west-1:111111111111:function:elevate-precheck"

    precheck.handler(EVENT, Ctx())
    event, kw = calls[0]
    assert event == EVENT
    assert kw["account"] == "111111111111"
    assert kw["send"] is precheck.put_response


def test_handler_answers_even_when_clients_cannot_be_built(monkeypatch):
    sent = Sent()
    monkeypatch.setattr(precheck, "put_response", sent)

    def broken(*_args, **_kwargs):
        raise RuntimeError("no region")

    monkeypatch.setattr(precheck.boto3, "client", broken)

    class Ctx:
        invoked_function_arn = "arn:aws:lambda:eu-west-1:111111111111:function:elevate-precheck"

    precheck.handler(EVENT, Ctx())
    assert sent[0][1]["Status"] == "FAILED"
    assert "no region" in sent[0][1]["Reason"]
