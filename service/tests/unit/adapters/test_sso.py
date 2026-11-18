import json

import boto3
import pytest
from botocore.stub import Stubber

from aws_elevate.adapters.sso_admin import SsoAdmin, SsoOperationFailed, SsoTimeout
from aws_elevate.adapters.sso_read import PsNotFound, SsoRead
from aws_elevate.core.models import SourcePs

INSTANCE = "arn:aws:sso:::instance/ssoins-1111111111111111"
SRC = "arn:aws:sso:::permissionSet/ssoins-1111111111111111/ps-1111111111111111"
JIT = "arn:aws:sso:::permissionSet/ssoins-1111111111111111/ps-2222222222222222"
KW = {"InstanceArn": INSTANCE, "PermissionSetArn": SRC}
JKW = {"InstanceArn": INSTANCE, "PermissionSetArn": JIT}
ACCOUNT = "111111111111"
USER = "u-1"
MANAGED = "arn:aws:iam::aws:policy/ReadOnlyAccess"
CMP = {"Name": "app-read", "Path": "/"}
BOUNDARY = {"ManagedPolicyArn": "arn:aws:iam::aws:policy/PowerUserAccess"}
INLINE = {
    "Version": "2012-10-17",
    "Statement": [{"Effect": "Allow", "Action": "s3:*", "Resource": "*"}],
}
REQ = "11111111-2222-3333-4444-555555555555"
NOT_FOUND_404 = "Received a 404 status error: EntitlementItem doesn't exist"

SOURCE = SourcePs(
    arn=SRC,
    name="ssm-write",
    managed=(MANAGED,),
    customer_managed=(CMP,),
    boundary=BOUNDARY,
    inline=INLINE,
)


@pytest.fixture
def stub():
    client = boto3.client("sso-admin", region_name="eu-west-1")
    with Stubber(client) as s:
        yield client, s
        s.assert_no_pending_responses()


@pytest.fixture
def reader(stub):
    client, s = stub
    return SsoRead(client, INSTANCE), s


@pytest.fixture
def admin(stub):
    client, s = stub
    reader = SsoRead(client, INSTANCE)
    return SsoAdmin(client, INSTANCE, reader, sleep=lambda _: None), s


def error(s, method, code, message="", params=None):
    s.add_client_error(
        method, service_error_code=code, service_message=message, expected_params=params
    )


def describe_ps(name, arn=SRC):
    return {"PermissionSet": {"Name": name, "PermissionSetArn": arn}}


# ---------- sso_read ----------


def stub_full_read(s, *, inline="", managed=(), cmp=(), boundary=None):
    s.add_response("describe_permission_set", describe_ps("ssm-write"), KW)
    s.add_response(
        "get_inline_policy_for_permission_set", {"InlinePolicy": inline} if inline else {}, KW
    )
    s.add_response(
        "list_managed_policies_in_permission_set",
        {"AttachedManagedPolicies": [{"Arn": a, "Name": "x"} for a in managed]},
        KW,
    )
    s.add_response(
        "list_customer_managed_policy_references_in_permission_set",
        {"CustomerManagedPolicyReferences": list(cmp)},
        KW,
    )
    if boundary is None:
        error(
            s, "get_permissions_boundary_for_permission_set", "ResourceNotFoundException", params=KW
        )
    else:
        s.add_response(
            "get_permissions_boundary_for_permission_set", {"PermissionsBoundary": boundary}, KW
        )


def test_read_source_returns_full_content(reader):
    r, s = reader
    stub_full_read(s, inline=json.dumps(INLINE), managed=[MANAGED], cmp=[CMP], boundary=BOUNDARY)
    assert r.read_source(SRC) == SOURCE


def test_read_source_without_inline_or_boundary(reader):
    r, s = reader
    stub_full_read(s)
    source = r.read_source(SRC)
    assert source.inline is None
    assert source.boundary is None
    assert source.managed == () and source.customer_managed == ()


def test_read_source_of_deleted_permission_set_raises(reader):
    r, s = reader
    error(s, "describe_permission_set", "ResourceNotFoundException", params=KW)
    with pytest.raises(PsNotFound):
        r.read_source(SRC)


def test_find_by_name(reader):
    r, s = reader
    s.add_response(
        "list_permission_sets", {"PermissionSets": [SRC, JIT]}, {"InstanceArn": INSTANCE}
    )
    s.add_response("describe_permission_set", describe_ps("ssm-write"), KW)
    s.add_response("describe_permission_set", describe_ps("JIT-abcde23456", JIT), JKW)
    assert r.find_by_name("JIT-abcde23456") == JIT


def test_find_by_name_missing(reader):
    r, s = reader
    s.add_response("list_permission_sets", {"PermissionSets": []}, {"InstanceArn": INSTANCE})
    assert r.find_by_name("JIT-abcde23456") is None


# ---------- sso_admin: create ----------


def stub_configure(s, *, attached=(), attached_cmp=()):
    s.add_response(
        "put_inline_policy_to_permission_set",
        {},
        {**JKW, "InlinePolicy": json.dumps(INLINE, separators=(",", ":"))},
    )
    s.add_response(
        "list_managed_policies_in_permission_set",
        {"AttachedManagedPolicies": [{"Arn": a, "Name": "x"} for a in attached]},
        JKW,
    )
    if MANAGED not in attached:
        s.add_response(
            "attach_managed_policy_to_permission_set", {}, {**JKW, "ManagedPolicyArn": MANAGED}
        )
    s.add_response(
        "list_customer_managed_policy_references_in_permission_set",
        {"CustomerManagedPolicyReferences": list(attached_cmp)},
        JKW,
    )
    if CMP not in attached_cmp:
        s.add_response(
            "attach_customer_managed_policy_reference_to_permission_set",
            {},
            {**JKW, "CustomerManagedPolicyReference": CMP},
        )
    s.add_response(
        "put_permissions_boundary_to_permission_set", {}, {**JKW, "PermissionsBoundary": BOUNDARY}
    )


CREATE = {
    "InstanceArn": INSTANCE,
    "Name": "JIT-abcde23456",
    "Description": "incident 42",
    "SessionDuration": "PT1H",
    "Tags": [{"Key": "jit", "Value": "true"}, {"Key": "jit:grantId", "Value": "abcde23456"}],
}
TAGS = {"jit": "true", "jit:grantId": "abcde23456"}


def create(a):
    arn = a.create_ephemeral("JIT-abcde23456", "incident 42", "PT1H", TAGS)
    a.configure(arn, INLINE, SOURCE)
    return arn


def test_create_ephemeral_copies_source(admin):
    a, s = admin
    s.add_response("create_permission_set", {"PermissionSet": {"PermissionSetArn": JIT}}, CREATE)
    stub_configure(s)
    assert create(a) == JIT


def test_create_ephemeral_omits_empty_description(admin):
    a, s = admin
    params = {k: v for k, v in CREATE.items() if k != "Description"}
    s.add_response("create_permission_set", {"PermissionSet": {"PermissionSetArn": JIT}}, params)
    stub_configure(s)
    a.configure(a.create_ephemeral("JIT-abcde23456", "", "PT1H", TAGS), INLINE, SOURCE)


def test_create_ephemeral_rerun_reuses_existing_and_skips_attached(admin):
    a, s = admin
    error(
        s, "create_permission_set", "ConflictException", "PermissionSet with name already exists."
    )
    s.add_response("list_permission_sets", {"PermissionSets": [JIT]}, {"InstanceArn": INSTANCE})
    s.add_response("describe_permission_set", describe_ps("JIT-abcde23456", JIT), JKW)
    stub_configure(s, attached=[MANAGED], attached_cmp=[CMP])
    assert create(a) == JIT


def test_create_ephemeral_retries_conflict_when_name_not_taken(admin):
    a, s = admin
    error(s, "create_permission_set", "ConflictException", "concurrent modification")
    s.add_response("list_permission_sets", {"PermissionSets": []}, {"InstanceArn": INSTANCE})
    s.add_response("create_permission_set", {"PermissionSet": {"PermissionSetArn": JIT}}, CREATE)
    stub_configure(s)
    assert create(a) == JIT


def test_throttling_is_retried(admin):
    a, s = admin
    error(s, "create_permission_set", "ThrottlingException")
    s.add_response("create_permission_set", {"PermissionSet": {"PermissionSetArn": JIT}}, CREATE)
    stub_configure(s)
    assert create(a) == JIT


def test_retries_stop_at_budget():
    client = boto3.client("sso-admin", region_name="eu-west-1")
    ticks = iter(range(0, 1000, 50))
    a = SsoAdmin(
        client,
        INSTANCE,
        SsoRead(client, INSTANCE),
        sleep=lambda _: None,
        clock=lambda: next(ticks),
        budget_s=120,
    )
    with Stubber(client) as s:
        for _ in range(5):
            error(s, "delete_permission_set", "ThrottlingException")
        with pytest.raises(Exception) as info:
            a.delete(JIT)
        assert "Throttling" in str(info.value)


def test_configure_without_boundary_or_policies(admin):
    a, s = admin
    bare = SourcePs(arn=SRC, name="x", managed=(), customer_managed=(), boundary=None, inline=None)
    s.add_response(
        "put_inline_policy_to_permission_set",
        {},
        {**JKW, "InlinePolicy": json.dumps(INLINE, separators=(",", ":"))},
    )
    s.add_response("list_managed_policies_in_permission_set", {"AttachedManagedPolicies": []}, JKW)
    s.add_response(
        "list_customer_managed_policy_references_in_permission_set",
        {"CustomerManagedPolicyReferences": []},
        JKW,
    )
    a.configure(JIT, INLINE, bare)


def test_validation_errors_are_not_retried(admin):
    a, s = admin
    error(s, "create_permission_set", "ValidationException", "bad name")
    with pytest.raises(Exception) as info:
        create(a)
    assert "ValidationException" in str(info.value)


# ---------- sso_admin: assign / unassign ----------

ASSIGN = {
    "InstanceArn": INSTANCE,
    "TargetId": ACCOUNT,
    "TargetType": "AWS_ACCOUNT",
    "PermissionSetArn": JIT,
    "PrincipalType": "USER",
    "PrincipalId": USER,
}


def status(key, value, reason=None):
    body = {"Status": value, "RequestId": REQ}
    if reason:
        body["FailureReason"] = reason
    return {key: body}


def test_assign_waits_for_success(admin):
    a, s = admin
    s.add_response(
        "create_account_assignment",
        status("AccountAssignmentCreationStatus", "IN_PROGRESS"),
        ASSIGN,
    )
    s.add_response(
        "describe_account_assignment_creation_status",
        status("AccountAssignmentCreationStatus", "IN_PROGRESS"),
        None,
    )
    s.add_response(
        "describe_account_assignment_creation_status",
        status("AccountAssignmentCreationStatus", "SUCCEEDED"),
        {"InstanceArn": INSTANCE, "AccountAssignmentCreationRequestId": REQ},
    )
    a.assign(JIT, ACCOUNT, USER)


def test_assign_failure_carries_aws_reason(admin):
    a, s = admin
    s.add_response(
        "create_account_assignment",
        status("AccountAssignmentCreationStatus", "IN_PROGRESS"),
        ASSIGN,
    )
    s.add_response(
        "describe_account_assignment_creation_status",
        status(
            "AccountAssignmentCreationStatus",
            "FAILED",
            "customer managed policy app-read not found",
        ),
        None,
    )
    with pytest.raises(SsoOperationFailed, match="app-read"):
        a.assign(JIT, ACCOUNT, USER)


def test_wait_gives_up_after_budget():
    client = boto3.client("sso-admin", region_name="eu-west-1")
    ticks = iter(range(0, 1000, 70))
    a = SsoAdmin(
        client,
        INSTANCE,
        SsoRead(client, INSTANCE),
        sleep=lambda _: None,
        clock=lambda: next(ticks),
        budget_s=120,
    )
    with Stubber(client) as s:
        s.add_response(
            "create_account_assignment",
            status("AccountAssignmentCreationStatus", "IN_PROGRESS"),
            ASSIGN,
        )
        for _ in range(3):
            s.add_response(
                "describe_account_assignment_creation_status",
                status("AccountAssignmentCreationStatus", "IN_PROGRESS"),
                None,
            )
        with pytest.raises(SsoTimeout):
            a.assign(JIT, ACCOUNT, USER)


def test_unassign_succeeds(admin):
    a, s = admin
    s.add_response(
        "delete_account_assignment",
        status("AccountAssignmentDeletionStatus", "IN_PROGRESS"),
        ASSIGN,
    )
    s.add_response(
        "describe_account_assignment_deletion_status",
        status("AccountAssignmentDeletionStatus", "SUCCEEDED"),
        {"InstanceArn": INSTANCE, "AccountAssignmentDeletionRequestId": REQ},
    )
    a.unassign(JIT, ACCOUNT, USER)


def test_unassign_already_gone_is_done(admin):
    a, s = admin
    s.add_response(
        "delete_account_assignment",
        status("AccountAssignmentDeletionStatus", "IN_PROGRESS"),
        ASSIGN,
    )
    s.add_response(
        "describe_account_assignment_deletion_status",
        status("AccountAssignmentDeletionStatus", "FAILED", NOT_FOUND_404),
        None,
    )
    a.unassign(JIT, ACCOUNT, USER)


def test_unassign_of_deleted_permission_set_is_done(admin):
    a, s = admin
    error(s, "delete_account_assignment", "ResourceNotFoundException", params=ASSIGN)
    a.unassign(JIT, ACCOUNT, USER)


def test_unassign_other_failure_raises(admin):
    a, s = admin
    s.add_response(
        "delete_account_assignment",
        status("AccountAssignmentDeletionStatus", "IN_PROGRESS"),
        ASSIGN,
    )
    s.add_response(
        "describe_account_assignment_deletion_status",
        status("AccountAssignmentDeletionStatus", "FAILED", "internal error"),
        None,
    )
    with pytest.raises(SsoOperationFailed):
        a.unassign(JIT, ACCOUNT, USER)


# ---------- sso_admin: rewrite / delete ----------

PROVISION = {
    "InstanceArn": INSTANCE,
    "PermissionSetArn": JIT,
    "TargetType": "AWS_ACCOUNT",
    "TargetId": ACCOUNT,
}


def stub_put_inline(s):
    s.add_response(
        "put_inline_policy_to_permission_set",
        {},
        {**JKW, "InlinePolicy": json.dumps(INLINE, separators=(",", ":"))},
    )


def test_rewrite_inline_reprovisions(admin):
    a, s = admin
    stub_put_inline(s)
    s.add_response(
        "provision_permission_set",
        status("PermissionSetProvisioningStatus", "IN_PROGRESS"),
        PROVISION,
    )
    s.add_response(
        "describe_permission_set_provisioning_status",
        status("PermissionSetProvisioningStatus", "SUCCEEDED"),
        {"InstanceArn": INSTANCE, "ProvisionPermissionSetRequestId": REQ},
    )
    a.rewrite_inline(JIT, ACCOUNT, INLINE)


def test_rewrite_inline_of_deleted_permission_set_is_done(admin):
    a, s = admin
    error(s, "put_inline_policy_to_permission_set", "ResourceNotFoundException")
    a.rewrite_inline(JIT, ACCOUNT, INLINE)


def test_rewrite_inline_when_no_longer_provisioned_is_done(admin):
    a, s = admin
    stub_put_inline(s)
    s.add_response(
        "provision_permission_set",
        status("PermissionSetProvisioningStatus", "IN_PROGRESS"),
        PROVISION,
    )
    s.add_response(
        "describe_permission_set_provisioning_status",
        status(
            "PermissionSetProvisioningStatus",
            "FAILED",
            "Received a 404 status error: Permission set provision not found",
        ),
        None,
    )
    a.rewrite_inline(JIT, ACCOUNT, INLINE)


def test_delete_permission_set(admin):
    a, s = admin
    s.add_response("delete_permission_set", {}, JKW)
    a.delete(JIT)


def test_delete_already_deleted_is_done(admin):
    a, s = admin
    error(s, "delete_permission_set", "ResourceNotFoundException", params=JKW)
    a.delete(JIT)


def test_delete_while_still_assigned_retries(admin):
    a, s = admin
    error(s, "delete_permission_set", "ConflictException", "has ApplicationProfile associated")
    s.add_response("delete_permission_set", {}, JKW)
    a.delete(JIT)
