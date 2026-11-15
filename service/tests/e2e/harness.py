"""End-to-end harness against a real test organization.

    python tests/e2e/harness.py up      # package, upload, create or update stack elevate-e2e
    python tests/e2e/harness.py down    # remove everything elevate-e2e* (always run it)

Environment: PROFILE (SSO profile of the test user, able to assume
OrganizationAccountAccessRole), SERVICE_ACCOUNT (Identity Center delegated admin),
TARGET_ACCOUNT (member account receiving grants), TEST_USER (Identity Center user name of
the profile's user). Optional: ALERT_EMAIL (default user@example.com).

Creates only elevate-e2e* resources: stack, code bucket, a source permission set, the
seeder Lambda (records written as the engine role, because the table policy admits no one
else), and JIT-e2e* permission sets made by the engine. `down` removes all of them,
including the resources the stack retains on deletion (table, AppConfig application)."""

import hashlib
import io
import json
import os
import sys
import time
import zipfile
from datetime import UTC, datetime
from pathlib import Path

import boto3
from botocore.exceptions import ClientError

STACK = "elevate-e2e"
SERVICE_DIR = Path(__file__).resolve().parents[2]
TEMPLATE = SERVICE_DIR.parent / "infra" / "elevate.yaml"
SOURCE_PS = "elevate-e2e-source"
SEEDER = "elevate-e2e-seeder"
GRANT_PREFIX = "e2e"  # e2e grant ids start with it, so cleanup never touches real grants
PROBE_ACTION = "iam:GetAccountSummary"

SEEDER_CODE = """
import boto3
db = boto3.client("dynamodb")

def h(event, context):
    if event["op"] == "write":
        db.transact_write_items(TransactItems=event["TransactItems"])
        return {}
    return db.get_item(TableName=event["TableName"], Key=event["Key"], ConsistentRead=True)
"""


def env(name: str, default: str | None = None) -> str:
    value = os.environ.get(name, default)
    if not value:
        raise SystemExit(f"missing environment variable {name}")
    return value


def log(message: str) -> None:
    print(f"[e2e {datetime.now(UTC):%H:%M:%S}] {message}", flush=True)


# ---------- sessions ----------


def human_session() -> boto3.Session:
    return boto3.Session(profile_name=env("PROFILE"))


def service_session(human: boto3.Session | None = None) -> boto3.Session:
    human = human or human_session()
    account = env("SERVICE_ACCOUNT")
    creds = human.client("sts").assume_role(
        RoleArn=f"arn:aws:iam::{account}:role/OrganizationAccountAccessRole",
        RoleSessionName="elevate-e2e",
    )["Credentials"]
    return boto3.Session(
        aws_access_key_id=creds["AccessKeyId"],
        aws_secret_access_key=creds["SecretAccessKey"],
        aws_session_token=creds["SessionToken"],
        region_name=human.region_name,
    )


# ---------- package and stack ----------


def package() -> bytes:
    """Zip of src/aws_elevate with fixed timestamps, so the same code gives the same zip."""
    buffer = io.BytesIO()
    root = SERVICE_DIR / "src"
    with zipfile.ZipFile(buffer, "w", zipfile.ZIP_DEFLATED) as zf:
        for path in sorted((root / "aws_elevate").rglob("*.py")):
            info = zipfile.ZipInfo(str(path.relative_to(root)), date_time=(2020, 1, 1, 0, 0, 0))
            info.external_attr = 0o644 << 16
            zf.writestr(info, path.read_bytes(), zipfile.ZIP_DEFLATED)
    return buffer.getvalue()


def bucket_name(session: boto3.Session) -> str:
    return f"elevate-e2e-{env('SERVICE_ACCOUNT')}-{session.region_name}"


def upload(session: boto3.Session) -> tuple[str, str]:
    s3, bucket, body = session.client("s3"), bucket_name(session), package()
    try:
        s3.create_bucket(
            Bucket=bucket,
            CreateBucketConfiguration={"LocationConstraint": session.region_name},
        )
        log(f"created bucket {bucket}")
    except ClientError as err:
        if err.response["Error"]["Code"] != "BucketAlreadyOwnedByYou":
            raise
    key = f"e2e/lambda-{hashlib.sha256(body).hexdigest()[:12]}.zip"
    s3.put_object(Bucket=bucket, Key=key, Body=body)
    log(f"uploaded s3://{bucket}/{key} ({len(body)} bytes)")
    return bucket, key


def stack_outputs(session: boto3.Session) -> dict:
    stack = session.client("cloudformation").describe_stacks(StackName=STACK)["Stacks"][0]
    return {o["OutputKey"]: o["OutputValue"] for o in stack.get("Outputs", [])}


def _stack_exists(cfn) -> bool:
    try:
        status = cfn.describe_stacks(StackName=STACK)["Stacks"][0]["StackStatus"]
    except ClientError as err:
        if "does not exist" in str(err):
            return False
        raise
    return status != "DELETE_COMPLETE"


def stack_up(session: boto3.Session) -> dict:
    bucket, key = upload(session)
    cfn = session.client("cloudformation")
    params = [
        {"ParameterKey": "AlertEmail", "ParameterValue": env("ALERT_EMAIL", "user@example.com")},
        {"ParameterKey": "CodeBucket", "ParameterValue": bucket},
        {"ParameterKey": "CodeKey", "ParameterValue": key},
    ]
    args = {
        "StackName": STACK,
        "TemplateBody": TEMPLATE.read_text(),
        "Parameters": params,
        "Capabilities": ["CAPABILITY_NAMED_IAM"],
    }
    started = time.monotonic()
    if _stack_exists(cfn):
        try:
            cfn.update_stack(**args)
        except ClientError as err:
            if "No updates are to be performed" not in str(err):
                raise
            log("stack already up to date")
            return stack_outputs(session)
        log("updating stack")
        cfn.get_waiter("stack_update_complete").wait(
            StackName=STACK, WaiterConfig={"Delay": 10, "MaxAttempts": 90}
        )
    else:
        log("creating stack")
        stack_id = cfn.create_stack(**args, OnFailure="DELETE")["StackId"]
        try:
            cfn.get_waiter("stack_create_complete").wait(
                StackName=STACK, WaiterConfig={"Delay": 10, "MaxAttempts": 90}
            )
        except Exception as err:
            for event in cfn.describe_stack_events(StackName=stack_id)["StackEvents"]:
                if event["ResourceStatus"] == "CREATE_FAILED":
                    log(f"{event['LogicalResourceId']}: {event.get('ResourceStatusReason')}")
            message = "stack creation failed; run `make e2e-down` to remove leftovers"
            raise SystemExit(message) from err
    log(f"stack ready in {time.monotonic() - started:.0f}s")
    return stack_outputs(session)


# ---------- seeder (records written as the engine role) ----------


def engine_role_arn(session: boto3.Session) -> str:
    return session.client("iam").get_role(RoleName=f"{STACK}-engine")["Role"]["Arn"]


def ensure_seeder(session: boto3.Session) -> None:
    lam = session.client("lambda")
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w") as zf:
        zf.writestr("seeder.py", SEEDER_CODE)
    try:
        lam.get_function(FunctionName=SEEDER)
        return
    except ClientError as err:
        if err.response["Error"]["Code"] != "ResourceNotFoundException":
            raise
    lam.create_function(
        FunctionName=SEEDER,
        Runtime="python3.12",
        Handler="seeder.h",
        Role=engine_role_arn(session),
        Code={"ZipFile": buffer.getvalue()},
        Timeout=30,
    )
    lam.get_waiter("function_active_v2").wait(FunctionName=SEEDER)
    log(f"created {SEEDER}")


def invoke(session: boto3.Session, function: str, payload: dict) -> dict:
    resp = session.client("lambda").invoke(FunctionName=function, Payload=json.dumps(payload))
    body = json.loads(resp["Payload"].read() or b"null")
    if resp.get("FunctionError"):
        raise RuntimeError(f"{function} failed: {body}")
    return body


# ---------- source permission set ----------


def sso_admin(session: boto3.Session):
    return session.client("sso-admin")


def ensure_source_ps(session: boto3.Session, instance_arn: str) -> str:
    """A plain (non-jit) permission set allowing only PROBE_ACTION: the rule's source."""
    sso = sso_admin(session)
    for page in sso.get_paginator("list_permission_sets").paginate(InstanceArn=instance_arn):
        for arn in page["PermissionSets"]:
            ps = sso.describe_permission_set(InstanceArn=instance_arn, PermissionSetArn=arn)
            if ps["PermissionSet"]["Name"] == SOURCE_PS:
                return arn
    arn = sso.create_permission_set(
        InstanceArn=instance_arn, Name=SOURCE_PS, SessionDuration="PT1H",
        Description="aws-elevate e2e source",
    )["PermissionSet"]["PermissionSetArn"]  # fmt: skip
    policy = {
        "Version": "2012-10-17",
        "Statement": [{"Effect": "Allow", "Action": PROBE_ACTION, "Resource": "*"}],
    }
    sso.put_inline_policy_to_permission_set(
        InstanceArn=instance_arn, PermissionSetArn=arn, InlinePolicy=json.dumps(policy)
    )
    log(f"created source permission set {SOURCE_PS}")
    return arn


# ---------- portal credentials and probe ----------


def sso_token(human: boto3.Session) -> str:
    from botocore.utils import JSONFileCache, SSOTokenLoader

    config = human._session.full_config
    profile = config["profiles"][env("PROFILE")]
    session_name = profile["sso_session"]
    start_url = config["sso_sessions"][session_name]["sso_start_url"]
    cache = JSONFileCache(os.path.expanduser("~/.aws/sso/cache"))
    token = SSOTokenLoader(cache=cache)(start_url, session_name=session_name)
    expires = datetime.fromisoformat(token["expiresAt"].replace("Z", "+00:00"))
    if expires <= datetime.now(UTC):
        raise SystemExit(f"SSO token expired: run `aws sso login --profile {env('PROFILE')}`")
    return token["accessToken"]


def role_credentials(human: boto3.Session, role: str, timeout_s: float = 120) -> dict:
    """Credentials of the ephemeral role from the access portal. Right after the
    assignment the portal refuses for a few seconds (spike C12): retry."""
    portal, token = human.client("sso"), sso_token(human)
    started = time.monotonic()
    while True:
        try:
            return portal.get_role_credentials(
                roleName=role, accountId=env("TARGET_ACCOUNT"), accessToken=token
            )["roleCredentials"]
        except ClientError as err:
            if time.monotonic() - started > timeout_s:
                raise
            if err.response["Error"]["Code"] not in (
                "ForbiddenException", "UnauthorizedException", "TooManyRequestsException",
            ):  # fmt: skip
                raise
            time.sleep(2)


def probe(creds: dict) -> str:
    """allowed | denied (policy deny) | invalid (credentials no longer accepted)."""
    iam = boto3.client(
        "iam",
        aws_access_key_id=creds["accessKeyId"],
        aws_secret_access_key=creds["secretAccessKey"],
        aws_session_token=creds["sessionToken"],
    )
    try:
        iam.get_account_summary()
        return "allowed"
    except ClientError as err:
        code = err.response["Error"]["Code"]
        if code in ("AccessDenied", "AccessDeniedException"):
            return "denied"
        if code in ("InvalidClientTokenId", "ExpiredToken"):
            return "invalid"
        raise


# ---------- teardown ----------


def _try(what: str, call, failures: list) -> None:
    try:
        call()
    except ClientError as err:
        code = err.response["Error"]["Code"]
        if code in ("ResourceNotFoundException", "NoSuchBucket", "NoSuchEntity",
                    "ResourceNotFound"):  # fmt: skip
            return
        failures.append(f"{what}: {err}")
        log(f"FAILED {what}: {err}")


def _wait_request(describe, key: str, id_key: str, instance: str, request_id: str) -> None:
    for _ in range(120):
        status = describe(InstanceArn=instance, **{id_key: request_id})[key]["Status"]
        if status != "IN_PROGRESS":
            return
        time.sleep(1)


def _delete_assignment(sso, instance: str, arn: str, account: str, ptype: str, pid: str) -> None:
    req = sso.delete_account_assignment(
        InstanceArn=instance, TargetId=account, TargetType="AWS_ACCOUNT",
        PermissionSetArn=arn, PrincipalType=ptype, PrincipalId=pid,
    )["AccountAssignmentDeletionStatus"]  # fmt: skip
    _wait_request(
        sso.describe_account_assignment_deletion_status, "AccountAssignmentDeletionStatus",
        "AccountAssignmentDeletionRequestId", instance, req["RequestId"],
    )  # fmt: skip


def _force_deprovision(sso, instance: str, arn: str, account: str, user_id: str) -> None:
    # Measured 2026-11-15: if the last unassignment FAILED (the redrive test denies
    # it), the permission set stays provisioned with no assignment and
    # DeletePermissionSet answers "ApplicationProfile associated". Removing a fresh
    # assignment again makes Identity Center deprovision it.
    req = sso.create_account_assignment(
        InstanceArn=instance, TargetId=account, TargetType="AWS_ACCOUNT",
        PermissionSetArn=arn, PrincipalType="USER", PrincipalId=user_id,
    )["AccountAssignmentCreationStatus"]  # fmt: skip
    _wait_request(
        sso.describe_account_assignment_creation_status, "AccountAssignmentCreationStatus",
        "AccountAssignmentCreationRequestId", instance, req["RequestId"],
    )  # fmt: skip
    _delete_assignment(sso, instance, arn, account, "USER", user_id)


def _remove_permission_set(
    sso, instance: str, arn: str, name: str, user_id: str, failures: list
) -> None:
    for page in sso.get_paginator("list_accounts_for_provisioned_permission_set").paginate(
        InstanceArn=instance, PermissionSetArn=arn
    ):
        for account in page["AccountIds"]:
            found = False
            for apage in sso.get_paginator("list_account_assignments").paginate(
                InstanceArn=instance, AccountId=account, PermissionSetArn=arn
            ):
                for a in apage["AccountAssignments"]:
                    found = True
                    _delete_assignment(
                        sso, instance, arn, account, a["PrincipalType"], a["PrincipalId"]
                    )
            if not found:
                log(f"{name} provisioned in {account} without assignments: deprovisioning")
                _force_deprovision(sso, instance, arn, account, user_id)
    for _ in range(10):
        try:
            sso.delete_permission_set(InstanceArn=instance, PermissionSetArn=arn)
            log(f"deleted permission set {name}")
            return
        except ClientError as err:
            if err.response["Error"]["Code"] == "ResourceNotFoundException":
                return
            if err.response["Error"]["Code"] != "ConflictException":
                failures.append(f"permission set {name}: {err}")
                return
            time.sleep(5)
    failures.append(f"permission set {name}: still in use")


def cleanup_permission_sets(session: boto3.Session, failures: list) -> None:
    sso = sso_admin(session)
    found = sso.list_instances()["Instances"][0]
    instance = found["InstanceArn"]
    user_id = session.client("identitystore").get_user_id(
        IdentityStoreId=found["IdentityStoreId"],
        AlternateIdentifier={
            "UniqueAttribute": {"AttributePath": "userName", "AttributeValue": env("TEST_USER")}
        },
    )["UserId"]
    for page in sso.get_paginator("list_permission_sets").paginate(InstanceArn=instance):
        for arn in page["PermissionSets"]:
            name = sso.describe_permission_set(InstanceArn=instance, PermissionSetArn=arn)[
                "PermissionSet"
            ]["Name"]
            if name.startswith(f"JIT-{GRANT_PREFIX}") or name == SOURCE_PS:
                _remove_permission_set(sso, instance, arn, name, user_id, failures)


def stack_down(session: boto3.Session) -> None:
    failures: list[str] = []
    cleanup_permission_sets(session, failures)
    lam, iam = session.client("lambda"), session.client("iam")
    _try("seeder", lambda: lam.delete_function(FunctionName=SEEDER), failures)
    _try(
        "e2e deny policy",
        lambda: iam.delete_role_policy(RoleName=f"{STACK}-engine", PolicyName="e2e-deny"),
        failures,
    )
    cfn = session.client("cloudformation")
    if _stack_exists(cfn):
        log("deleting stack")
        cfn.delete_stack(StackName=STACK)
        cfn.get_waiter("stack_delete_complete").wait(
            StackName=STACK, WaiterConfig={"Delay": 10, "MaxAttempts": 90}
        )
    # Retained by the stack on purpose (design §12.3): remove them for the test stack.
    ddb = session.client("dynamodb")
    _try("table", lambda: ddb.delete_table(TableName=f"{STACK}-grants") and log(
        f"deleted table {STACK}-grants"), failures)  # fmt: skip
    appconfig = session.client("appconfig")
    for app in appconfig.list_applications()["Items"]:
        if app["Name"] != STACK:
            continue
        for profile in appconfig.list_configuration_profiles(ApplicationId=app["Id"])["Items"]:
            versions = appconfig.list_hosted_configuration_versions(
                ApplicationId=app["Id"], ConfigurationProfileId=profile["Id"]
            )["Items"]
            for v in versions:
                _try(
                    f"config version {v['VersionNumber']}",
                    lambda v=v, p=profile, a=app: appconfig.delete_hosted_configuration_version(
                        ApplicationId=a["Id"], ConfigurationProfileId=p["Id"],
                        VersionNumber=v["VersionNumber"],
                    ),
                    failures,
                )  # fmt: skip
            _try(
                "config profile",
                lambda p=profile, a=app: appconfig.delete_configuration_profile(
                    ApplicationId=a["Id"], ConfigurationProfileId=p["Id"]
                ),
                failures,
            )
        _try("config application", lambda a=app: appconfig.delete_application(
            ApplicationId=a["Id"]), failures)  # fmt: skip
    logs = session.client("logs")
    for group in logs.describe_log_groups(logGroupNamePrefix=f"/aws/lambda/{STACK}")["logGroups"]:
        _try(group["logGroupName"], lambda g=group: logs.delete_log_group(
            logGroupName=g["logGroupName"]), failures)  # fmt: skip
    s3 = session.client("s3")
    bucket = bucket_name(session)
    try:
        for page in s3.get_paginator("list_object_versions").paginate(Bucket=bucket):
            for obj in page.get("Versions", []) + page.get("DeleteMarkers", []):
                s3.delete_object(Bucket=bucket, Key=obj["Key"], VersionId=obj["VersionId"])
        s3.delete_bucket(Bucket=bucket)
        log(f"deleted bucket {bucket}")
    except ClientError as err:
        if err.response["Error"]["Code"] != "NoSuchBucket":
            failures.append(f"bucket: {err}")
    if failures:
        raise SystemExit("teardown incomplete:\n  " + "\n  ".join(failures))
    log("teardown complete: no elevate-e2e resources left")


if __name__ == "__main__":
    command = sys.argv[1] if len(sys.argv) > 1 else ""
    if command == "up":
        svc = service_session()
        stack_up(svc)
        ensure_seeder(svc)
    elif command == "down":
        stack_down(service_session())
    else:
        raise SystemExit(__doc__)
