import boto3
import pytest
from botocore.stub import Stubber

from aws_elevate.adapters.iam_roles import IamRoles
from aws_elevate.adapters.identity_store import IdentityStore

STORE = "d-1111111111"
STORE_ARN = f"arn:aws:identitystore::111111111111:identitystore/{STORE}"
SSO_ROLES = "arn:aws:iam::111111111111:role/aws-reserved/sso.amazonaws.com/eu-west-1"


@pytest.fixture
def store():
    client = boto3.client("identitystore", region_name="eu-west-1")
    with Stubber(client) as stub:
        yield IdentityStore(client, STORE), stub
        stub.assert_no_pending_responses()


def test_user_id(store):
    s, stub = store
    stub.add_response(
        "get_user_id",
        {
            "IdentityStoreId": STORE,
            "UserId": "u-1",
            "UserArn": f"{STORE_ARN}/user/u-1",
        },
        {
            "IdentityStoreId": STORE,
            "AlternateIdentifier": {
                "UniqueAttribute": {
                    "AttributePath": "userName",
                    "AttributeValue": "dev@example.com",
                }
            },
        },
    )
    assert s.user_id("dev@example.com") == "u-1"


def test_unknown_user(store):
    s, stub = store
    stub.add_client_error("get_user_id", service_error_code="ResourceNotFoundException")
    assert s.user_id("ghost@example.com") is None


def membership(group):
    return {
        "IdentityStoreId": STORE,
        "MembershipId": f"m-{group}",
        "MembershipArn": f"{STORE_ARN}/membership/m-{group}",
        "GroupId": group,
        "MemberId": {"UserId": "u-1"},
    }


def test_group_ids_across_pages(store):
    s, stub = store
    args = {"IdentityStoreId": STORE, "MemberId": {"UserId": "u-1"}}
    stub.add_response(
        "list_group_memberships_for_member",
        {"GroupMemberships": [membership("g-1")], "NextToken": "t"},
        args,
    )
    stub.add_response(
        "list_group_memberships_for_member",
        {"GroupMemberships": [membership("g-2")]},
        {**args, "NextToken": "t"},
    )
    assert s.group_ids("u-1") == frozenset({"g-1", "g-2"})


@pytest.fixture
def iam():
    client = boto3.client("iam", region_name="us-east-1")
    with Stubber(client) as stub:
        yield IamRoles(client), stub
        stub.assert_no_pending_responses()


ROLE = "AWSReservedSSO_ElevateAdmin_0123456789abcdef"


def role_response():
    return {
        "Role": {
            "Path": "/aws-reserved/sso.amazonaws.com/eu-west-1/",
            "RoleName": ROLE,
            "RoleId": "AROAEXAMPLEADMIN0000",
            "Arn": f"{SSO_ROLES}/{ROLE}",
            "CreateDate": "2026-11-15T10:00:00Z",
        }
    }


def test_role_read_once_per_container(iam):
    roles, stub = iam
    stub.add_response("get_role", role_response(), {"RoleName": ROLE})
    expected = ("AROAEXAMPLEADMIN0000", "/aws-reserved/sso.amazonaws.com/eu-west-1/")
    assert roles.role(ROLE) == expected
    assert roles.role(ROLE) == expected  # no second call: the stub would fail


def test_missing_role(iam):
    roles, stub = iam
    stub.add_client_error("get_role", service_error_code="NoSuchEntity")
    assert roles.role(ROLE) is None
