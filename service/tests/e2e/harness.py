"""End-to-end harness against a real test organization.

    python tests/e2e/harness.py up      # package, upload, install or update stack elevate-e2e
    python tests/e2e/harness.py down    # remove everything elevate-e2e* (always run it)

Environment: PROFILE (SSO profile of the test user, able to assume
OrganizationAccountAccessRole), SERVICE_ACCOUNT (Identity Center delegated admin),
TARGET_ACCOUNT (member account receiving grants), TEST_USER (Identity Center user name of
the profile's user). Optional: ALERT_EMAIL (default user@example.com); E2E_APPROVAL=1 to
run the approval by a second user, with APPROVER_PROFILE (SSO profile logged in as
elevate-e2e-approver, with its own sso-session). The approval run needs a manual first login
of that user, so it is opt-in: use it only when the approval path changes.

Creates only elevate-e2e* resources, plus the permission set ElevateConsole (the
maintainer's console access to the service account during tests): stack, code bucket, a
source permission set, the
seeder Lambda (records written as the engine role, because the table policy admits no one
else), the groups elevate-e2e-admins and elevate-e2e-requesters with the test user in
both and elevate-e2e-auditors, with E2E_APPROVAL=1 the user elevate-e2e-approver in the
admin group (a request cannot be approved by its requester), with E2E_USERS=1 also
elevate-e2e-requester and elevate-e2e-auditor, one per group, for manual UI checks (each
first login is done by hand: one-time password from the console),
and the JIT-* permission sets the engine makes for e2e grants. The stack also
creates the service permission sets ElevateRequester, ElevateAdmin and ElevateAuditor,
under their real names (the client looks them up by name; the maintainer approved it for
the test organization). `down` removes all of them, including the resources the stack retains on
deletion (table, AppConfig application).

A new stack is installed with the client (`aws-elevate install`, built into
build/aws-elevate) at the previous infrastructure version, PREVIOUS_INFRA: the same code
with another INFRA_VERSION. test_upgrade.py, run first, updates it to the current code
with a grant open (design §12.2). The install signs as OrganizationAccountAccessRole of
the service account, through a temporary AWS config with one more profile."""

import hashlib
import io
import json
import os
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from datetime import UTC, datetime
from pathlib import Path

import boto3
from botocore.auth import SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.exceptions import ClientError

STACK = "elevate-e2e"
PREVIOUS_INFRA = "0.0.0"  # what a new e2e stack runs until test_upgrade updates it
CLI = Path(
    os.environ.get("ELEVATE_CLI", Path(__file__).resolve().parents[3] / "build" / "aws-elevate")
)
SERVICE_DIR = Path(__file__).resolve().parents[2]
TEMPLATE = SERVICE_DIR.parent / "infra" / "elevate.yaml"
SOURCE_PS = "elevate-e2e-source"
# The maintainer's way into the console of the service account during tests (reset the
# approver's password, read logs): Identity Center users and groups, read-only elsewhere.
CONSOLE_PS = "ElevateConsole"
CONSOLE_POLICIES = (
    "arn:aws:iam::aws:policy/AWSSSODirectoryAdministrator",
    "arn:aws:iam::aws:policy/AWSSSOMemberAccountAdministrator",
    "arn:aws:iam::aws:policy/ReadOnlyAccess",
)
SEEDER = "elevate-e2e-seeder"
GRANT_PREFIX = "e2e"  # grant ids written by the engine tests start with it
RULE_PREFIX = "e2e-"  # every e2e rule id: cleanup finds the JIT-* of api grants by tag
RULE_ID = "e2e-probe"  # auto, the test user's requester group
MANUAL_RULE_ID = "e2e-manual"  # manual, approved by the admin group the test user is in
OTHER_RULE_ID = "e2e-other"  # a group the test user is not in
ADMIN_GROUP = "elevate-e2e-admins"
APPROVER_USER = "elevate-e2e-approver"
REQUESTER_GROUP = "elevate-e2e-requesters"
AUDITOR_GROUP = "elevate-e2e-auditors"
REQUESTER_USER = "elevate-e2e-requester"
AUDITOR_USER = "elevate-e2e-auditor"
# Extra people for manual UI checks (E2E_USERS=1), one role each: user -> group.
PEOPLE = {APPROVER_USER: ADMIN_GROUP, REQUESTER_USER: REQUESTER_GROUP, AUDITOR_USER: AUDITOR_GROUP}
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


def approval_enabled() -> bool:
    """The approval by a second user runs only on request: its login is manual."""
    return os.environ.get("E2E_APPROVAL") == "1"


def users_enabled() -> bool:
    """E2E_USERS=1 also creates a pure requester and an auditor for manual UI checks."""
    return os.environ.get("E2E_USERS") == "1"


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


# ---------- groups ----------


def _store(session: boto3.Session) -> tuple[object, str]:
    store_id = session.client("sso-admin").list_instances()["Instances"][0]["IdentityStoreId"]
    return session.client("identitystore"), store_id


def find_user(session: boto3.Session, name: str) -> str | None:
    ids, store_id = _store(session)
    try:
        return ids.get_user_id(
            IdentityStoreId=store_id,
            AlternateIdentifier={
                "UniqueAttribute": {"AttributePath": "userName", "AttributeValue": name}
            },
        )["UserId"]
    except ClientError as err:
        if err.response["Error"]["Code"] != "ResourceNotFoundException":
            raise
        return None


def e2e_user_id(session: boto3.Session) -> str:
    user = find_user(session, env("TEST_USER"))
    if user is None:
        raise SystemExit(f"Identity Center user {env('TEST_USER')} not found")
    return user


def _add_member(session: boto3.Session, group: str, user: str) -> None:
    ids, store_id = _store(session)
    try:
        ids.create_group_membership(
            IdentityStoreId=store_id, GroupId=group, MemberId={"UserId": user}
        )
    except ClientError as err:
        if err.response["Error"]["Code"] != "ConflictException":
            raise


def ensure_person(session: boto3.Session, name: str, group: str) -> None:
    ids, store_id = _store(session)
    user = find_user(session, name)
    if user is None:
        user = ids.create_user(
            IdentityStoreId=store_id,
            UserName=name,
            DisplayName=name.replace("-", " "),
            Name={"GivenName": "elevate", "FamilyName": name.removeprefix("elevate-")},
            Emails=[{"Value": f"{name}@example.com", "Type": "work", "Primary": True}],
        )["UserId"]
        log(f"created user {name}: set its password in the console and log in")
    _add_member(session, group, user)


def delete_people(session: boto3.Session, failures: list) -> None:
    ids, store_id = _store(session)
    for name in PEOPLE:
        user = find_user(session, name)
        if user is None:
            continue
        _try(f"user {name}", lambda u=user: ids.delete_user(
            IdentityStoreId=store_id, UserId=u), failures)  # fmt: skip
        log(f"deleted user {name}")


def find_group(session: boto3.Session, name: str) -> str | None:
    ids, store_id = _store(session)
    try:
        return ids.get_group_id(
            IdentityStoreId=store_id,
            AlternateIdentifier={
                "UniqueAttribute": {"AttributePath": "displayName", "AttributeValue": name}
            },
        )["GroupId"]
    except ClientError as err:
        if err.response["Error"]["Code"] != "ResourceNotFoundException":
            raise
        return None


def ensure_groups(session: boto3.Session) -> dict[str, str]:
    """Group ids by name. The test user is in the admin and requester groups; the auditor
    group holds only the auditor of E2E_USERS=1."""
    ids, store_id = _store(session)
    user = e2e_user_id(session)
    found = {}
    for name in (ADMIN_GROUP, REQUESTER_GROUP, AUDITOR_GROUP):
        group = find_group(session, name)
        if group is None:
            group = ids.create_group(
                IdentityStoreId=store_id, DisplayName=name, Description="aws-elevate e2e"
            )["GroupId"]
            log(f"created group {name}")
        if name != AUDITOR_GROUP:
            _add_member(session, group, user)
        found[name] = group
    if approval_enabled() or users_enabled():
        ensure_person(session, APPROVER_USER, found[ADMIN_GROUP])
    if users_enabled():
        for name in (REQUESTER_USER, AUDITOR_USER):
            ensure_person(session, name, found[PEOPLE[name]])
    return found


def delete_groups(session: boto3.Session, failures: list) -> None:
    ids, store_id = _store(session)
    for name in (ADMIN_GROUP, REQUESTER_GROUP, AUDITOR_GROUP):
        group = find_group(session, name)
        if group is None:
            continue
        for page in ids.get_paginator("list_group_memberships").paginate(
            IdentityStoreId=store_id, GroupId=group
        ):
            for m in page["GroupMemberships"]:
                _try(
                    f"membership in {name}",
                    lambda m=m: ids.delete_group_membership(
                        IdentityStoreId=store_id, MembershipId=m["MembershipId"]
                    ),
                    failures,
                )
        _try(
            f"group {name}",
            lambda g=group: ids.delete_group(IdentityStoreId=store_id, GroupId=g),
            failures,
        )
        log(f"deleted group {name}")


# ---------- package and stack ----------


def package(infra_version: str | None = None) -> bytes:
    """Zip of src/aws_elevate with fixed timestamps, so the same code gives the same zip.
    infra_version, if given, replaces INFRA_VERSION: the release before this one."""
    buffer = io.BytesIO()
    root = SERVICE_DIR / "src"
    with zipfile.ZipFile(buffer, "w", zipfile.ZIP_DEFLATED) as zf:
        for path in sorted((root / "aws_elevate").rglob("*.py")):
            body = path.read_bytes()
            if infra_version and path.name == "version.py":
                body, n = re.subn(rb'INFRA_VERSION = "[^"]*"',
                                  f'INFRA_VERSION = "{infra_version}"'.encode(), body)  # fmt: skip
                assert n == 1, "INFRA_VERSION not found in version.py"
            info = zipfile.ZipInfo(str(path.relative_to(root)), date_time=(2020, 1, 1, 0, 0, 0))
            info.external_attr = 0o644 << 16
            zf.writestr(info, body, zipfile.ZIP_DEFLATED)
    return buffer.getvalue()


def bucket_name(session: boto3.Session) -> str:
    return f"elevate-e2e-{env('SERVICE_ACCOUNT')}-{session.region_name}"


def template_url(session: boto3.Session) -> str:
    return f"https://{bucket_name(session)}.s3.{session.region_name}.amazonaws.com/e2e/elevate.yaml"


def upload(session: boto3.Session, infra_version: str | None = None) -> tuple[str, str]:
    s3, bucket, body = session.client("s3"), bucket_name(session), package(infra_version)
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
    # The template is over the 51,200-byte limit of TemplateBody; from S3 it may be 1 MB,
    # as in a release.
    s3.put_object(Bucket=bucket, Key="e2e/elevate.yaml", Body=TEMPLATE.read_bytes())
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
    """Updates the stack to the current code, or installs it at PREVIOUS_INFRA."""
    cfn = session.client("cloudformation")
    if not _stack_exists(cfn):
        install(session)
        return stack_outputs(session)
    bucket, key = upload(session)
    groups = ensure_groups(session)
    params = [
        {"ParameterKey": "AlertEmail", "ParameterValue": env("ALERT_EMAIL", "user@example.com")},
        {"ParameterKey": "CodeBucket", "ParameterValue": bucket},
        {"ParameterKey": "CodeKey", "ParameterValue": key},
        {"ParameterKey": "AdminGroupId", "ParameterValue": groups[ADMIN_GROUP]},
        {"ParameterKey": "RequesterGroupId", "ParameterValue": groups[REQUESTER_GROUP]},
        {"ParameterKey": "AuditorGroupId", "ParameterValue": groups[AUDITOR_GROUP]},
    ]
    _update(cfn, params, template_url(session))
    return stack_outputs(session)


def _update(cfn, params: list[dict], url: str) -> None:
    started = time.monotonic()
    try:
        cfn.update_stack(StackName=STACK, TemplateURL=url, Parameters=params,
                         Capabilities=["CAPABILITY_NAMED_IAM"])  # fmt: skip
    except ClientError as err:
        if "No updates are to be performed" not in str(err):
            raise
        log("stack already up to date")
        return
    log("updating stack")
    cfn.get_waiter("stack_update_complete").wait(
        StackName=STACK, WaiterConfig={"Delay": 10, "MaxAttempts": 90}
    )
    log(f"stack updated in {time.monotonic() - started:.0f}s")


def upgrade(session: boto3.Session) -> None:
    """The update an admin runs from `admin updates --preview` (design §12.3): every
    parameter keeps its value except CodeKey, the package of the new release."""
    bucket, key = upload(session)
    cfn = session.client("cloudformation")
    stack = cfn.describe_stacks(StackName=STACK)["Stacks"][0]
    params = [
        {"ParameterKey": p["ParameterKey"], "UsePreviousValue": True}
        for p in stack["Parameters"]
        if p["ParameterKey"] != "CodeKey"
    ]
    params.append({"ParameterKey": "CodeKey", "ParameterValue": key})
    _update(cfn, params, template_url(session))


def install(session: boto3.Session) -> None:
    """`aws-elevate install` of the previous release, as an admin of the service account."""
    if not CLI.is_file():
        raise SystemExit(f"{CLI} is missing: run `make e2e-up`, which builds it")
    bucket, key = upload(session, PREVIOUS_INFRA)
    ensure_groups(session)
    human = human_session()
    role = f"arn:aws:iam::{env('SERVICE_ACCOUNT')}:role/OrganizationAccountAccessRole"
    real = Path(os.environ.get("AWS_CONFIG_FILE", Path.home() / ".aws" / "config"))
    with tempfile.TemporaryDirectory() as tmp:
        config = Path(tmp) / "config"
        config.write_text(
            real.read_text()
            + "\n[profile elevate-e2e-install]\n"
            + f"role_arn = {role}\n"
            + f"source_profile = {human.profile_name}\n"
            + f"region = {session.region_name}\n"
        )
        environ = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
        environ.update(AWS_CONFIG_FILE=str(config), XDG_CONFIG_HOME=tmp)
        cmd = [
            str(CLI), "install", "--profile", "elevate-e2e-install",
            "--region", session.region_name, "--stack-name", STACK,
            "--template-url", template_url(session), "--code-bucket", bucket, "--code-key", key,
            "--admin-group", ADMIN_GROUP, "--requester-group", REQUESTER_GROUP,
            "--auditor-group", AUDITOR_GROUP,
            "--email", env("ALERT_EMAIL", "user@example.com"),
        ]  # fmt: skip
        log(f"installing {STACK} {PREVIOUS_INFRA} with aws-elevate install")
        started = time.monotonic()
        result = subprocess.run(cmd, env=environ, capture_output=True, text=True, timeout=1200)
    print(result.stdout, end="", flush=True)
    if result.returncode != 0:
        print(result.stderr, end="", file=sys.stderr, flush=True)
        raise SystemExit("aws-elevate install failed; run `make e2e-down` to remove leftovers")
    log(f"installed in {time.monotonic() - started:.0f}s")


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


def ensure_console_ps(session: boto3.Session) -> None:
    """CONSOLE_PS assigned to the test user in the service account."""
    sso = sso_admin(session)
    instance = sso.list_instances()["Instances"][0]["InstanceArn"]
    arn = None
    for page in sso.get_paginator("list_permission_sets").paginate(InstanceArn=instance):
        for candidate in page["PermissionSets"]:
            ps = sso.describe_permission_set(InstanceArn=instance, PermissionSetArn=candidate)
            if ps["PermissionSet"]["Name"] == CONSOLE_PS:
                arn = candidate
    if arn is None:
        arn = sso.create_permission_set(
            InstanceArn=instance, Name=CONSOLE_PS, SessionDuration="PT4H",
            Description="aws-elevate e2e: maintainer console access",
        )["PermissionSet"]["PermissionSetArn"]  # fmt: skip
        for policy in CONSOLE_POLICIES:
            sso.attach_managed_policy_to_permission_set(
                InstanceArn=instance, PermissionSetArn=arn, ManagedPolicyArn=policy
            )
        log(f"created permission set {CONSOLE_PS}")
    req = sso.create_account_assignment(
        InstanceArn=instance, TargetId=env("SERVICE_ACCOUNT"), TargetType="AWS_ACCOUNT",
        PermissionSetArn=arn, PrincipalType="USER", PrincipalId=e2e_user_id(session),
    )["AccountAssignmentCreationStatus"]  # fmt: skip
    _wait_request(
        sso.describe_account_assignment_creation_status, "AccountAssignmentCreationStatus",
        "AccountAssignmentCreationRequestId", instance, req["RequestId"],
    )  # fmt: skip
    log(f"{CONSOLE_PS} assigned to {env('TEST_USER')} in {env('SERVICE_ACCOUNT')}")


# ---------- portal credentials and probe ----------


def sso_token(human: boto3.Session) -> str:
    from botocore.utils import JSONFileCache, SSOTokenLoader

    config = human._session.full_config
    profile = config["profiles"][human.profile_name]
    session_name = profile["sso_session"]
    start_url = config["sso_sessions"][session_name]["sso_start_url"]
    cache = JSONFileCache(os.path.expanduser("~/.aws/sso/cache"))
    token = SSOTokenLoader(cache=cache)(start_url, session_name=session_name)
    expires = datetime.fromisoformat(token["expiresAt"].replace("Z", "+00:00"))
    if expires <= datetime.now(UTC):
        raise SystemExit(f"SSO token expired: run `aws sso login --profile {human.profile_name}`")
    return token["accessToken"]


def role_credentials(
    human: boto3.Session, role: str, timeout_s: float = 120, account: str | None = None
) -> dict:
    """Credentials of a role of the test user from the access portal, by default in the
    target account. Right after the assignment the portal refuses for a few seconds
    (spike C12): retry."""
    portal, token = human.client("sso"), sso_token(human)
    started = time.monotonic()
    while True:
        try:
            return portal.get_role_credentials(
                roleName=role, accountId=account or env("TARGET_ACCOUNT"), accessToken=token
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


# ---------- signed api calls ----------


def call_api(
    url: str, creds, method: str, path: str, body=None, region: str = "eu-west-1"
) -> tuple[int, dict]:
    """(status, JSON body) of a SigV4-signed call, as the client makes it. `creds` is a
    botocore Credentials object."""
    from aws_elevate.version import INFRA_VERSION

    data = json.dumps(body).encode() if body is not None else None
    request = AWSRequest(
        method=method,
        url=f"{url}/v1/{path}",
        data=data,
        headers={"Content-Type": "application/json", "X-Elevate-Client-Version": INFRA_VERSION},
    )
    SigV4Auth(creds, "execute-api", region).add_auth(request)
    prepared = urllib.request.Request(
        request.url, data=data, method=method, headers=dict(request.headers.items())
    )
    try:
        with urllib.request.urlopen(prepared, timeout=30) as resp:
            return resp.status, json.loads(resp.read() or b"null")
    except urllib.error.HTTPError as err:
        return err.code, json.loads(err.read() or b"null")


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


def _e2e_api_grant(sso, instance: str, arn: str, name: str) -> bool:
    """JIT-* of a grant made through api in an e2e test: random id, e2e rule tag."""
    if not name.startswith("JIT-"):
        return False
    tags = sso.list_tags_for_resource(InstanceArn=instance, ResourceArn=arn)["Tags"]
    return any(t["Key"] == "jit:rule" and t["Value"].startswith(RULE_PREFIX) for t in tags)


def cleanup_permission_sets(session: boto3.Session, failures: list) -> None:
    sso = sso_admin(session)
    found = sso.list_instances()["Instances"][0]
    instance = found["InstanceArn"]
    user_id = e2e_user_id(session)
    for page in sso.get_paginator("list_permission_sets").paginate(InstanceArn=instance):
        for arn in page["PermissionSets"]:
            name = sso.describe_permission_set(InstanceArn=instance, PermissionSetArn=arn)[
                "PermissionSet"
            ]["Name"]
            if (
                name.startswith(f"JIT-{GRANT_PREFIX}")
                or name in (SOURCE_PS, CONSOLE_PS)
                or _e2e_api_grant(sso, instance, arn, name)
            ):
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
    # The stack's group assignments are gone with it: the groups can go now.
    delete_people(session, failures)
    delete_groups(session, failures)
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
        ensure_console_ps(svc)
    elif command == "down":
        stack_down(service_session())
    else:
        raise SystemExit(__doc__)
