"""Custom resource run first by the stack (design §12.1): stops the install with a clear
message when the region is not the Identity Center home region or the account is not the
delegated administrator.

It must answer CloudFormation on every path, exceptions included: a custom resource that
never answers blocks the stack for up to an hour (it happened in spike C). The resource
also has ServiceTimeout: 60 in the template."""

import json
import urllib.request

import boto3

_PHYSICAL_ID = "elevate-precheck"
_MAX_REASON = 1000


class CheckFailed(Exception):
    """A precondition is not met; the message is shown in the stack events."""


def check(sso, org, account: str) -> dict:
    instances = sso.list_instances()["Instances"]
    if not instances:
        raise CheckFailed(
            "Identity Center has no instance in this region: deploy the stack in the "
            "Identity Center home region"
        )
    instance = instances[0]
    if org.describe_organization()["Organization"]["MasterAccountId"] == account:
        raise CheckFailed(
            "this is the management account: deploy in the Identity Center delegated "
            "administrator account instead"
        )
    try:
        sso.list_permission_sets(InstanceArn=instance["InstanceArn"], MaxResults=1)
    except Exception as err:
        raise CheckFailed(
            "this account cannot administer Identity Center: deploy in the delegated "
            f"administrator account ({type(err).__name__})"
        ) from err
    return {
        "InstanceArn": instance["InstanceArn"],
        "InstanceId": instance["InstanceArn"].rsplit("/", 1)[1],
        "IdentityStoreId": instance["IdentityStoreId"],
    }


def put_response(url: str, body: str) -> None:
    request = urllib.request.Request(
        url, data=body.encode(), method="PUT", headers={"Content-Type": ""}
    )
    with urllib.request.urlopen(request, timeout=10):
        pass


def _answer(event: dict, send, status: str, reason: str, data: dict) -> None:
    body = {
        "Status": status,
        "Reason": reason[:_MAX_REASON],
        "PhysicalResourceId": _PHYSICAL_ID,
        "StackId": event["StackId"],
        "RequestId": event["RequestId"],
        "LogicalResourceId": event["LogicalResourceId"],
        "Data": data,
    }
    send(event["ResponseURL"], json.dumps(body))


def respond_to(event: dict, *, sso, org, account: str, send) -> None:
    try:
        data = {} if event["RequestType"] == "Delete" else check(sso, org, account)
    except CheckFailed as err:
        _answer(event, send, "FAILED", str(err), {})
        return
    except Exception as err:
        _answer(event, send, "FAILED", f"precheck error: {type(err).__name__}: {err}", {})
        return
    _answer(event, send, "SUCCESS", "ok", data)


def handler(event, context):
    try:
        account = context.invoked_function_arn.split(":")[4]
        sso = boto3.client("sso-admin")
        org = boto3.client("organizations")
    except Exception as err:
        _answer(event, put_response, "FAILED", f"precheck error: {type(err).__name__}: {err}", {})
        return
    respond_to(event, sso=sso, org=org, account=account, send=put_response)
