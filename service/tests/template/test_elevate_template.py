"""What each role of infra/elevate.yaml can and cannot do, and the guarantees
the template must keep."""

import hashlib
import json
import re
from pathlib import Path

import pytest
from cfnlint.decode import decode

from aws_elevate.statemachine import definition

TEMPLATE_PATH = Path(__file__).resolve().parents[3] / "infra" / "elevate.yaml"
TEMPLATE, _errors = decode(str(TEMPLATE_PATH))
RESOURCES = TEMPLATE["Resources"]

SSO_READ_PREFIXES = ("Describe", "Get", "List")


def resolve_key_conditions(value, has_key: bool):
    """Resolve the `!If [HasKmsKey, a, b]` nodes as CloudFormation does; anything else is kept."""
    if isinstance(value, dict):
        branches = value.get("Fn::If")
        if len(value) == 1 and branches and branches[0] == "HasKmsKey":
            chosen = branches[1] if has_key else branches[2]
            return resolve_key_conditions(chosen, has_key)
        resolved = {k: resolve_key_conditions(v, has_key) for k, v in value.items()}
        return {k: v for k, v in resolved.items() if v != {"Ref": "AWS::NoValue"}}
    if isinstance(value, list):
        out = [resolve_key_conditions(v, has_key) for v in value]
        return [v for v in out if v != {"Ref": "AWS::NoValue"}]
    return value


def statements(role: str, has_key: bool = True) -> list[dict]:
    out = []
    for policy in RESOURCES[role]["Properties"]["Policies"]:
        out += resolve_key_conditions(policy["PolicyDocument"]["Statement"], has_key)
    return out


def as_list(value) -> list:
    return value if isinstance(value, list) else [value]


def text(value) -> str:
    """Resource expressions are intrinsic functions; compare on their JSON text."""
    return json.dumps(value)


def roles() -> list[str]:
    return [name for name, r in RESOURCES.items() if r["Type"] == "AWS::IAM::Role"]


def test_template_decodes_without_errors():
    assert not _errors


# ---------- engine role ----------


def engine_allows():
    return [s for s in statements("EngineRole") if s["Effect"] == "Allow"]


def is_sso_write(action: str) -> bool:
    return action.startswith("sso:") and not action.split(":")[1].startswith(SSO_READ_PREFIXES)


def test_engine_writes_permission_sets_only_with_jit_tag():
    for stmt in engine_allows():
        writes = [a for a in as_list(stmt["Action"]) if is_sso_write(a)]
        if not writes:
            continue
        for resource in as_list(stmt["Resource"]):
            if "permissionSet/" in text(resource):
                cond = stmt.get("Condition", {}).get("StringEquals", {})
                tagged = cond.get("aws:ResourceTag/jit") == "true" or (
                    cond.get("aws:RequestTag/jit") == "true"
                )
                assert tagged, (stmt["Sid"], writes)


def test_engine_has_no_sso_wildcards():
    for stmt in statements("EngineRole"):
        for action in as_list(stmt["Action"]):
            assert action not in ("sso:*", "*"), stmt["Sid"]


def test_engine_scoped_to_the_instance_found_by_the_precheck():
    for stmt in engine_allows():
        for resource in as_list(stmt["Resource"]):
            if "permissionSet/" in text(resource):
                assert "${Precheck.InstanceId}" in text(resource), stmt["Sid"]


def test_engine_can_never_untag():
    denies = [s for s in statements("EngineRole") if s["Effect"] == "Deny"]
    assert any("sso:UntagResource" in as_list(s["Action"]) and s["Resource"] == "*" for s in denies)
    for stmt in engine_allows():
        assert "sso:UntagResource" not in as_list(stmt["Action"])


def test_engine_tags_only_jit_keys():
    tag = [s for s in engine_allows() if s.get("Sid") == "TagOnlyJitOnCreate"][0]
    assert tag["Condition"]["ForAllValues:StringLike"]["aws:TagKeys"] == ["jit", "jit:*"]


def test_engine_cannot_touch_iam_lambda_or_states():
    for stmt in engine_allows():
        for action in as_list(stmt["Action"]):
            if stmt["Sid"] == "DecryptGrantsWithCustomerKey":
                assert action == "kms:Decrypt"
                continue
            assert action.split(":")[0] in ("sso", "dynamodb", "appconfig", "logs"), action


def test_engine_reads_rules_of_this_application_only():
    # AWS authorizes GetHostedConfigurationVersion against the application and the
    # configuration profile ARNs (service authorization reference); a hosted
    # version has no ARN of its own.
    read = [s for s in engine_allows() if s.get("Sid") == "ReadRules"][0]
    assert as_list(read["Action"]) == ["appconfig:GetHostedConfigurationVersion"]
    resources = [text(r) for r in as_list(read["Resource"])]
    assert len(resources) == 2
    assert all("application/${RulesApplication}" in r for r in resources)
    assert any(r.endswith('application/${RulesApplication}"}') for r in resources)
    assert any(
        r.endswith('configurationprofile/${RulesProfile.ConfigurationProfileId}"}')
        for r in resources
    )


# ---------- other roles ----------


def test_state_machine_role_only_invokes_engine_and_publishes_events():
    actions = {a for s in statements("StateMachineRole") for a in as_list(s["Action"])}
    assert actions == {"lambda:InvokeFunction", "events:PutEvents"}


def test_precheck_role_is_read_only():
    actions = {a for s in statements("PrecheckRole") for a in as_list(s["Action"])}
    assert actions == {
        "sso:ListInstances",
        "sso:ListPermissionSets",
        "organizations:DescribeOrganization",
        "logs:CreateLogStream",
        "logs:PutLogEvents",
    }


def test_every_role_is_tested():
    assert set(roles()) == {"EngineRole", "StateMachineRole", "PrecheckRole", "ApiRole"}


# ---------- table ----------


def test_table_is_retained():
    table = RESOURCES["GrantsTable"]
    assert table["DeletionPolicy"] == "Retain"
    assert table["UpdateReplacePolicy"] == "Retain"


def test_table_denies_data_access_to_everyone_but_the_service_roles_by_exact_arn():
    policy = RESOURCES["GrantsTable"]["Properties"]["ResourcePolicy"]["PolicyDocument"]
    (deny,) = policy["Statement"]
    assert deny["Effect"] == "Deny" and deny["Principal"] == "*"
    assert {"dynamodb:GetItem", "dynamodb:Query", "dynamodb:PutItem"} <= set(deny["Action"])
    allowed = deny["Condition"]["ArnNotEquals"]["aws:PrincipalArn"]
    assert "*" not in text(allowed).replace("${AWS::", "")
    assert "role/elevate/${AWS::StackName}-engine" in text(allowed)
    assert "role/elevate/${AWS::StackName}-api" in text(allowed)


def test_engine_role_name_matches_the_table_exception():
    assert RESOURCES["EngineRole"]["Properties"]["RoleName"] == {
        "Fn::Sub": "${AWS::StackName}-engine"
    }
    assert RESOURCES["EngineRole"]["Properties"]["Path"] == "/elevate/"


def test_table_has_the_three_indexes_and_ttl():
    props = RESOURCES["GrantsTable"]["Properties"]
    assert [i["IndexName"] for i in props["GlobalSecondaryIndexes"]] == [
        "byUser",
        "byMonth",
        "open",
    ]
    assert props["TimeToLiveSpecification"] == {"AttributeName": "ttl", "Enabled": True}


# ---------- precheck ----------


def depends_on(name: str) -> set[str]:
    """Resources `name` depends on, explicitly or through Ref/GetAtt/Sub."""
    resource = RESOURCES[name]
    deps = set(as_list(resource.get("DependsOn", [])))
    body = text(resource.get("Properties", {}))
    for other in RESOURCES:
        if (
            f'"Ref": "{other}"' in body
            or f'"Fn::GetAtt": ["{other}"' in body
            or f"${{{other}." in body
            or f"${{{other}}}" in body
        ):
            deps.add(other)
    return deps


def test_precheck_answers_within_sixty_seconds():
    props = RESOURCES["Precheck"]["Properties"]
    assert props["ServiceTimeout"] == 60


@pytest.mark.parametrize(
    "name",
    sorted(n for n in RESOURCES if n not in {"Precheck", "PrecheckFunction", "PrecheckRole",
                                              "PrecheckLogs"}),
)  # fmt: skip
def test_everything_waits_for_the_precheck(name):
    seen, todo = set(), [name]
    while todo:
        current = todo.pop()
        if current in seen:
            continue
        seen.add(current)
        todo += depends_on(current)
    assert "Precheck" in seen


# ---------- lambdas and state machine ----------


@pytest.mark.parametrize(
    ("name", "handler"),
    [
        ("EngineFunction", "aws_elevate.engine.handler.handler"),
        ("PrecheckFunction", "aws_elevate.precheck.handler.handler"),
        ("ApiFunction", "aws_elevate.api.handler.handler"),
    ],
)
def test_lambda_runtime_and_handler(name, handler):
    props = RESOURCES[name]["Properties"]
    assert props["Runtime"] == "python3.12"
    assert props["Handler"] == handler


def test_state_machine_definition_is_the_python_one():
    props = RESOURCES["GrantStateMachine"]["Properties"]
    # A string, not a mapping: CloudFormation rejects null values (ResultPath: null).
    assert json.loads(props["DefinitionString"]) == definition("${EngineArn}", "${BusName}")
    assert set(props["DefinitionSubstitutions"]) == {"EngineArn", "BusName"}


def test_alert_and_approval_rules():
    patterns = {
        n: RESOURCES[n]["Properties"]["EventPattern"]
        for n in ("GrantErrorRule", "ConfigRejectedRule", "ManualRequestRule")
    }
    assert patterns["GrantErrorRule"]["detail-type"] == ["elevate.grant.error"]
    assert patterns["ConfigRejectedRule"]["detail-type"] == ["elevate.config.rejected"]
    assert patterns["ManualRequestRule"]["detail"] == {"approval": ["manual"]}
    approvals = RESOURCES["ApprovalsTopic"]["Properties"]
    assert "Subscription" not in approvals


def test_sns_topic_policy_statements_name_one_topic_each():
    # SNS refuses a statement covering several topics (seen deploying the e2e stack).
    for name, resource in RESOURCES.items():
        if resource["Type"] != "AWS::SNS::TopicPolicy":
            continue
        props = resource["Properties"]
        assert len(props["Topics"]) == 1, name
        for stmt in props["PolicyDocument"]["Statement"]:
            assert not isinstance(stmt["Resource"], list), name


# ---------- api role: reads untrusted input, never writes to sso ----------

SCHEMA_PATH = TEMPLATE_PATH.parent / "rules.schema.json"


# What adapters.sso_read.read_source calls (rule limits and the summary of what the requester gets).
API_PS_READS = (
    "sso:DescribePermissionSet",
    "sso:GetInlinePolicyForPermissionSet",
    "sso:ListManagedPoliciesInPermissionSet",
    "sso:ListCustomerManagedPolicyReferencesInPermissionSet",
    "sso:GetPermissionsBoundaryForPermissionSet",
)


def api_actions() -> set[str]:
    return {
        a for s in statements("ApiRole") if s["Effect"] == "Allow" for a in as_list(s["Action"])
    }


def api_statement(sid: str) -> dict:
    (stmt,) = [s for s in statements("ApiRole") if s.get("Sid") == sid]
    return stmt


def test_api_has_no_sso_write_action():
    assert not [a for a in api_actions() if is_sso_write(a) or a in ("sso:*", "*")]


def test_api_actions_are_exactly_the_design_ones():
    assert api_actions() == {
        "dynamodb:GetItem",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:DeleteItem",
        "dynamodb:ConditionCheckItem",
        "dynamodb:Query",
        "appconfig:GetHostedConfigurationVersion",
        "appconfig:ListHostedConfigurationVersions",
        "appconfig:ValidateConfiguration",
        "cloudtrail:LookupEvents",
        "states:StartExecution",
        "states:SendTaskSuccess",
        "states:RedriveExecution",
        "identitystore:GetUserId",
        "identitystore:ListGroupMembershipsForMember",
        "iam:GetRole",
        *API_PS_READS,
        "events:PutEvents",
        "logs:CreateLogStream",
        "logs:PutLogEvents",
        "kms:Decrypt",  # only under HasKmsKey, see the customer key tests
    }


def test_api_reads_permission_sets_of_this_instance_only():
    stmt = api_statement("ReadPermissionSets")
    assert sorted(as_list(stmt["Action"])) == sorted(API_PS_READS)
    assert as_list(stmt["Resource"]) == [
        {"Fn::GetAtt": ["Precheck", "InstanceArn"]},
        {"Fn::Sub": "arn:${AWS::Partition}:sso:::permissionSet/${Precheck.InstanceId}/*"},
    ]
    assert not [s for s in statements("ApiRole") if "sso:" in text(s) and s["Sid"] != stmt["Sid"]]


def test_api_reads_this_identity_store_whatever_account_owns_it():
    # Measured 2026-11-15: the store ARN carries the account that owns the Identity
    # Center instance (the management account), not the delegated admin running the stack.
    stmt = api_statement("ReadUsersAndGroups")
    store, *rest = as_list(stmt["Resource"])
    sub = "arn:${AWS::Partition}:identitystore::*:identitystore/${Precheck.IdentityStoreId}"
    assert store == {"Fn::Sub": sub}
    assert "${AWS::AccountId}" not in text(rest)


def test_api_never_scans_the_table():
    assert "dynamodb:Scan" not in api_actions()
    table = api_statement("GrantsTable")
    assert all("GrantsTable" in text(r) for r in as_list(table["Resource"]))


def test_api_reads_iam_roles_only_of_identity_center_service_roles():
    stmt = api_statement("CompareRoleId")
    assert as_list(stmt["Action"]) == ["iam:GetRole"]
    for resource in as_list(stmt["Resource"]):
        assert "role/aws-reserved/sso.amazonaws.com/" in text(resource)
        assert "AWSReservedSSO_" in text(resource)


def test_api_publishes_only_the_config_rejected_event_on_its_bus():
    stmt = api_statement("ConfigRejectedEvent")
    assert stmt["Resource"] == {"Fn::GetAtt": ["EventBus", "Arn"]}
    assert stmt["Condition"] == {"StringEquals": {"events:detail-type": "elevate.config.rejected"}}


def test_api_starts_and_redrives_only_this_state_machine():
    start = api_statement("StartGrant")
    assert start["Resource"] == {"Ref": "GrantStateMachine"}
    redrive = api_statement("RedriveGrant")
    assert "execution:${GrantStateMachine.Name}:*" in text(redrive["Resource"])


def test_api_reads_rules_of_this_application_only():
    stmt = api_statement("ReadRules")
    assert all("${RulesApplication}" in text(r) for r in as_list(stmt["Resource"]))


def test_api_role_name_matches_the_table_exception():
    props = RESOURCES["ApiRole"]["Properties"]
    assert props["RoleName"] == {"Fn::Sub": "${AWS::StackName}-api"}
    assert props["Path"] == "/elevate/"


# ---------- API Gateway ----------

SERVICE_ROLES = [
    "arn:${AWS::Partition}:iam::${AWS::AccountId}:role/aws-reserved/sso.amazonaws.com/*"
    "AWSReservedSSO_ElevateRequester_*",
    "arn:${AWS::Partition}:iam::${AWS::AccountId}:role/aws-reserved/sso.amazonaws.com/*"
    "AWSReservedSSO_ElevateAdmin_*",
    "arn:${AWS::Partition}:iam::${AWS::AccountId}:role/aws-reserved/sso.amazonaws.com/*"
    "AWSReservedSSO_ElevateAuditor_*",
]


def api_fingerprint() -> str:
    """Short hash of what an API Gateway deployment publishes: the API with its resource
    policy, its paths and its methods."""
    published = {
        name: r
        for name, r in RESOURCES.items()
        if r["Type"].startswith("AWS::ApiGateway::") and r["Type"] != "AWS::ApiGateway::Deployment"
    }
    return hashlib.sha256(json.dumps(published, sort_keys=True).encode()).hexdigest()[:8].upper()


def test_a_change_to_the_api_gets_a_new_deployment():
    # A stack update changes the API but republishes it only through a new deployment;
    # with a fixed logical id the stage keeps serving the first one forever (seen in the
    # e2e stack: a new role stayed denied after the update). The id carries a fingerprint
    # of the API, so any change to it forces a rename and CloudFormation deploys again.
    (deployment,) = [n for n, r in RESOURCES.items() if r["Type"] == "AWS::ApiGateway::Deployment"]
    assert deployment == f"ApiDeployment{api_fingerprint()}", (
        f"the API changed: rename {deployment} to ApiDeployment{api_fingerprint()}"
    )


def test_resource_policy_allows_the_two_roles_and_denies_everyone_else_explicitly():
    (allow, deny) = RESOURCES["RestApi"]["Properties"]["Policy"]["Statement"]
    assert allow["Effect"] == "Allow"
    assert allow["Condition"] == {
        "ArnLike": {"aws:PrincipalArn": [{"Fn::Sub": r} for r in SERVICE_ROLES]}
    }
    # Same account: an Allow alone refuses nobody.
    assert deny["Effect"] == "Deny" and deny["Principal"] == {"AWS": "*"}
    assert deny["Action"] == "execute-api:Invoke"
    assert deny["Resource"] == "execute-api:/*"
    assert deny["Condition"] == {
        "ArnNotLike": {"aws:PrincipalArn": [{"Fn::Sub": r} for r in SERVICE_ROLES]}
    }


def test_every_api_method_needs_a_signature():
    methods = [r for r in RESOURCES.values() if r["Type"] == "AWS::ApiGateway::Method"]
    assert methods
    assert all(m["Properties"]["AuthorizationType"] == "AWS_IAM" for m in methods)


def test_api_url_is_published_under_the_fixed_parameter_name():
    assert RESOURCES["ApiUrlParameter"]["Properties"]["Name"] == "/elevate/api-url"


# ---------- rules profile ----------


def test_rules_profile_validates_with_the_rules_schema():
    (validator,) = RESOURCES["RulesProfile"]["Properties"]["Validators"]
    assert validator["Type"] == "JSON_SCHEMA"
    assert json.loads(validator["Content"]) == json.loads(SCHEMA_PATH.read_text())


# ---------- service permission sets ----------


def ps_statements(name: str) -> list[dict]:
    return RESOURCES[name]["Properties"]["InlinePolicy"]["Statement"]


def ps_actions(name: str) -> set[str]:
    return {a for s in ps_statements(name) for a in as_list(s["Action"])}


def test_service_permission_sets_have_fixed_names_and_no_jit_tag():
    for name, ps_name in (
        ("RequesterPs", "ElevateRequester"),
        ("AdminPs", "ElevateAdmin"),
        ("AuditorPs", "ElevateAuditor"),
    ):
        props = RESOURCES[name]["Properties"]
        assert props["Name"] == ps_name
        assert "jit" not in text(props.get("Tags", []))


def test_requester_can_only_call_the_api_and_read_its_address():
    assert ps_actions("RequesterPs") == {"execute-api:Invoke", "ssm:GetParameter"}


def test_auditor_has_exactly_the_requester_policy():
    # The name tells the auditor what they are in the portal; what they may do through the
    # API is decided by the service from their groups.
    assert ps_statements("AuditorPs") == ps_statements("RequesterPs")


def test_admin_cannot_write_permission_sets_iam_lambda_or_apply_stack_updates():
    actions = ps_actions("AdminPs")
    assert {"execute-api:Invoke", "ssm:GetParameter"} <= actions
    assert "appconfig:CreateHostedConfigurationVersion" in actions
    forbidden = [
        a
        for a in actions
        if is_sso_write(a)
        or a.startswith(("iam:", "lambda:"))
        or a in ("cloudformation:ExecuteChangeSet", "cloudformation:UpdateStack")
        or a.endswith(":*")
    ]
    assert forbidden == []


RELEASE_BUCKETS_PATH = TEMPLATE_PATH.parents[1] / "contract" / "release-buckets.json"
RELEASE_BUCKETS = json.loads(RELEASE_BUCKETS_PATH.read_text())
RELEASE_BUCKET = {"Fn::FindInMap": ["ReleaseBuckets", {"Ref": "AWS::Region"}, "Bucket"]}


def test_release_buckets_come_from_the_contract_map_never_from_the_region_name():
    # A name derived from the region could be taken by someone else in a region where
    # aws-elevate is not published: only mapped buckets are read, and a
    # region without one fails the stack before anything is created.
    assert TEMPLATE["Mappings"]["ReleaseBuckets"] == {
        region: {"Bucket": bucket} for region, bucket in RELEASE_BUCKETS.items()
    }
    assert "aws-elevate-releases-${AWS::Region}" not in TEMPLATE_PATH.read_text()
    for name, res in RESOURCES.items():
        if res["Type"] == "AWS::Lambda::Function":
            assert res["Properties"]["Code"]["S3Bucket"] == {
                "Fn::If": ["OfficialCode", RELEASE_BUCKET, {"Ref": "CodeBucket"}]
            }, name


def test_admin_previews_updates_only_of_this_stack_and_from_the_release_bucket():
    # Create and read change sets, never apply them.
    statements = ps_statements("AdminPs")
    cfn = [
        s for s in statements if any(a.startswith("cloudformation:") for a in as_list(s["Action"]))
    ]
    assert {a for s in cfn for a in as_list(s["Action"])} == {
        "cloudformation:DescribeStacks",
        "cloudformation:CreateChangeSet",
        "cloudformation:DescribeChangeSet",
        "cloudformation:ListChangeSets",
        "cloudformation:GetTemplateSummary",
    }
    release_templates = {
        "StringLike": {
            "cloudformation:TemplateUrl": {
                "Fn::Sub": [
                    "https://${Bucket}.s3.${AWS::Region}.amazonaws.com/*",
                    {"Bucket": RELEASE_BUCKET},
                ]
            }
        }
    }
    # GetTemplateSummary on a template URL names no stack, so it cannot be scoped to this
    # stack's ARN: it is read-only and limited to release templates instead.
    (summary,) = [s for s in cfn if "cloudformation:GetTemplateSummary" in as_list(s["Action"])]
    assert as_list(summary["Action"]) == ["cloudformation:GetTemplateSummary"]
    assert summary["Resource"] == "*"
    assert summary["Condition"] == release_templates
    for s in cfn:
        if s is not summary:
            assert s["Resource"] == {"Ref": "AWS::StackId"}
    (create,) = [s for s in cfn if "cloudformation:CreateChangeSet" in as_list(s["Action"])]
    assert as_list(create["Action"]) == ["cloudformation:CreateChangeSet"]
    assert create["Condition"] == release_templates
    (read,) = [s for s in statements if "s3:GetObject" in as_list(s["Action"])]
    assert as_list(read["Action"]) == ["s3:GetObject"]
    assert read["Resource"] == {
        "Fn::Sub": ["arn:${AWS::Partition}:s3:::${Bucket}/*", {"Bucket": RELEASE_BUCKET}]
    }


def test_auditors_get_api_access_only_when_the_group_is_given():
    # An auditor only reads, but without a service permission set cannot call
    # the API at all: seen in the UI pass with an auditor outside the requesters group.
    assert TEMPLATE["Conditions"]["HasAuditors"] == {
        "Fn::Not": [{"Fn::Equals": [{"Ref": "AuditorGroupId"}, ""]}]
    }
    assert RESOURCES["AuditorPs"]["Condition"] == "HasAuditors"
    assert RESOURCES["AuditorAssignment"]["Condition"] == "HasAuditors"


def test_service_permission_sets_are_assigned_to_the_groups_by_id_in_this_account():
    for assignment, ps, group in (
        ("RequesterAssignment", "RequesterPs", "RequesterGroupId"),
        ("AdminAssignment", "AdminPs", "AdminGroupId"),
        ("AuditorAssignment", "AuditorPs", "AuditorGroupId"),
    ):
        props = RESOURCES[assignment]["Properties"]
        assert props["PermissionSetArn"] == {"Fn::GetAtt": [ps, "PermissionSetArn"]}
        assert props["PrincipalType"] == "GROUP"
        assert props["PrincipalId"] == {"Ref": group}
        assert props["TargetId"] == {"Ref": "AWS::AccountId"}


def test_api_function_gets_the_permission_set_names_and_parameters():
    env = RESOURCES["ApiFunction"]["Properties"]["Environment"]["Variables"]
    assert env["REQUESTER_PS_NAME"] == RESOURCES["RequesterPs"]["Properties"]["Name"]
    assert env["ADMIN_PS_NAME"] == RESOURCES["AdminPs"]["Properties"]["Name"]
    assert env["AUDITOR_PS_NAME"] == RESOURCES["AuditorPs"]["Properties"]["Name"]
    assert env["MAX_OPEN_GRANTS_PER_USER"] == {"Ref": "MaxOpenGrantsPerUser"}
    assert env["CONFIG_VERSIONS_LISTED"] == {"Ref": "ConfigVersionsListed"}
    assert env["AUDITOR_GROUP_ID"] == {"Ref": "AuditorGroupId"}
    assert env["SSO_INSTANCE_ARN"] == {"Fn::GetAtt": ["Precheck", "InstanceArn"]}


def test_api_function_knows_what_the_admin_client_reads_with_its_own_credentials():
    # Returned by GET /me to admins: the resources ElevateAdmin may act on.
    env = RESOURCES["ApiFunction"]["Properties"]["Environment"]["Variables"]
    assert env["STACK_ID"] == {"Ref": "AWS::StackId"}
    assert env["ALERTS_TOPIC_ARN"] == {"Ref": "AlertsTopic"}
    assert env["APPROVALS_TOPIC_ARN"] == {"Ref": "ApprovalsTopic"}


@pytest.mark.parametrize("function", ["ApiFunction", "EngineFunction"])
def test_api_and_engine_know_the_reserved_accounts(function):
    # The check before publishing and the activation refuse them as targets.
    env = RESOURCES[function]["Properties"]["Environment"]["Variables"]
    assert env["MANAGEMENT_ACCOUNT_ID"] == {"Fn::GetAtt": ["Precheck", "ManagementAccountId"]}
    assert env["SERVICE_ACCOUNT_ID"] == {"Ref": "AWS::AccountId"}


def test_install_parameters():
    params = TEMPLATE["Parameters"]
    assert {"AdminGroupId", "RequesterGroupId"} <= set(params)
    assert params["AuditorGroupId"]["Default"] == ""
    assert params["MaxOpenGrantsPerUser"]["Default"] == 5
    assert params["ConfigVersionsListed"]["Default"] == 50
    assert params["ConfigVersionsListed"]["MinValue"] == 1


def test_history_retention_parameter():
    param = TEMPLATE["Parameters"]["HistoryRetentionMonths"]
    assert param["Type"] == "Number"
    assert param["Default"] == 13
    assert param["MinValue"] == 1
    assert param["MaxValue"] == 120
    assert (
        INTERFACE["ParameterLabels"]["HistoryRetentionMonths"]["default"] == "History kept (months)"
    )
    groups = {g["Label"]["default"]: g["Parameters"] for g in INTERFACE["ParameterGroups"]}
    assert groups["Audit and data protection (optional)"][0] == "HistoryRetentionMonths"


def test_pitr_parameter():
    param = TEMPLATE["Parameters"]["PointInTimeRecovery"]
    assert param["Type"] == "String"
    assert param["Default"] == "Disabled"
    assert param["AllowedValues"] == ["Disabled", "Enabled"]
    label = INTERFACE["ParameterLabels"]["PointInTimeRecovery"]["default"]
    assert label == "Point-in-time recovery"
    groups = {g["Label"]["default"]: g["Parameters"] for g in INTERFACE["ParameterGroups"]}
    assert groups["Audit and data protection (optional)"] == [
        "HistoryRetentionMonths",
        "PointInTimeRecovery",
        "KmsKeyArn",
    ]


def test_table_pitr_follows_the_parameter():
    assert TEMPLATE["Conditions"]["PitrEnabled"] == {
        "Fn::Equals": [{"Ref": "PointInTimeRecovery"}, "Enabled"]
    }
    spec = RESOURCES["GrantsTable"]["Properties"]["PointInTimeRecoverySpecification"]
    assert spec["PointInTimeRecoveryEnabled"] == {"Fn::If": ["PitrEnabled", True, False]}


@pytest.mark.parametrize("function", ["ApiFunction", "EngineFunction"])
def test_api_and_engine_get_the_history_retention(function):
    env = RESOURCES[function]["Properties"]["Environment"]["Variables"]
    assert env["HISTORY_RETENTION_MONTHS"] == {"Ref": "HistoryRetentionMonths"}


def test_organization_name_is_optional_and_reaches_the_api():
    # Shown in the interface only; free text, so no markup characters.
    param = TEMPLATE["Parameters"]["OrganizationName"]
    assert param["Type"] == "String"
    assert param["Default"] == ""
    assert param["MaxLength"] == 64
    assert param["AllowedPattern"] == "^[^<>]*$"
    env = RESOURCES["ApiFunction"]["Properties"]["Environment"]["Variables"]
    assert env["ORGANIZATION_NAME"] == {"Ref": "OrganizationName"}


INTERFACE = TEMPLATE.get("Metadata", {}).get("AWS::CloudFormation::Interface", {})


def test_every_parameter_is_in_exactly_one_console_group():
    listed = [p for group in INTERFACE.get("ParameterGroups", []) for p in group["Parameters"]]
    assert sorted(listed) == sorted(TEMPLATE["Parameters"])
    labels = INTERFACE.get("ParameterLabels", {})
    for name in TEMPLATE["Parameters"]:
        assert labels.get(name, {}).get("default"), name


def test_console_groups_in_order():
    groups = [g["Label"]["default"] for g in INTERFACE.get("ParameterGroups", [])]
    assert groups == [
        "Who uses Elevate (required)",
        "Alerts and interface",
        "Limits (optional, defaults are fine)",
        "Integrations (optional)",
        "Audit and data protection (optional)",
        "For testing only: leave as they are",
    ]


# ---------- customer KMS key ----------

KMS_ARN_PATTERN = TEMPLATE["Parameters"]["KmsKeyArn"]["AllowedPattern"]
LOG_GROUPS = ["PrecheckLogs", "EngineLogs", "ApiLogs"]


@pytest.mark.parametrize(
    "value,ok",
    [
        ("", True),
        ("arn:aws:kms:eu-west-1:111111111111:key/1234abcd-12ab-34cd-56ef-1234567890ab", True),
        ("alias/x", False),
        ("arn:aws:kms:eu-west-1:111111111111:alias/x", False),
        ("arn:aws:s3:::bucket", False),
    ],
)
def test_kms_key_parameter_accepts_only_a_key_arn(value, ok):
    assert bool(re.fullmatch(KMS_ARN_PATTERN, value)) is ok


def test_kms_key_parameter_is_optional_and_in_the_data_protection_group():
    param = TEMPLATE["Parameters"]["KmsKeyArn"]
    assert param["Type"] == "String"
    assert param["Default"] == ""
    assert "README" in param["Description"]
    assert INTERFACE["ParameterLabels"]["KmsKeyArn"]["default"] == "Customer KMS key ARN"
    groups = {g["Label"]["default"]: g["Parameters"] for g in INTERFACE["ParameterGroups"]}
    assert groups["Audit and data protection (optional)"][-1] == "KmsKeyArn"
    assert TEMPLATE["Conditions"]["HasKmsKey"] == {
        "Fn::Not": [{"Fn::Equals": [{"Ref": "KmsKeyArn"}, ""]}]
    }


def test_customer_key_encrypts_the_table_and_the_three_log_groups():
    sse = RESOURCES["GrantsTable"]["Properties"]["SSESpecification"]
    assert sse == {
        "Fn::If": [
            "HasKmsKey",
            {"SSEEnabled": True, "SSEType": "KMS", "KMSMasterKeyId": {"Ref": "KmsKeyArn"}},
            {"Ref": "AWS::NoValue"},
        ]
    }
    for name in LOG_GROUPS:
        key = RESOURCES[name]["Properties"]["KmsKeyId"]
        assert key == {"Fn::If": ["HasKmsKey", {"Ref": "KmsKeyArn"}, {"Ref": "AWS::NoValue"}]}, name


@pytest.mark.parametrize("role", ["ApiRole", "EngineRole"])
def test_api_and_engine_only_decrypt_with_the_customer_key_through_dynamodb(role):
    kms = [
        s
        for s in statements(role)
        if any(a.startswith("kms:") for a in as_list(s.get("Action", [])))
    ]
    assert kms == [
        {
            "Sid": "DecryptGrantsWithCustomerKey",
            "Effect": "Allow",
            "Action": "kms:Decrypt",
            "Resource": {"Ref": "KmsKeyArn"},
            "Condition": {
                "StringEquals": {
                    "kms:ViaService": {"Fn::Sub": "dynamodb.${AWS::Region}.amazonaws.com"}
                }
            },
        }
    ]
    raw = [
        s
        for p in RESOURCES[role]["Properties"]["Policies"]
        for s in p["PolicyDocument"]["Statement"]
    ]
    (guarded,) = [s for s in raw if "DecryptGrantsWithCustomerKey" in json.dumps(s)]
    assert guarded["Fn::If"][0] == "HasKmsKey"
    assert guarded["Fn::If"][2] == {"Ref": "AWS::NoValue"}


def test_without_a_key_nothing_mentions_kms():
    resolved = json.dumps(
        resolve_key_conditions(
            {
                k: v
                for k, v in RESOURCES.items()
                if k not in ("GrantsTable", *LOG_GROUPS, "ApiRole", "EngineRole")
            },
            False,
        )
    )
    assert "kms" not in resolved.lower()
    for name in ("GrantsTable", *LOG_GROUPS, "ApiRole", "EngineRole"):
        props = resolve_key_conditions(RESOURCES[name]["Properties"], False)
        assert "kms" not in json.dumps(props).lower(), name
        assert "SSESpecification" not in props and "KmsKeyId" not in props
