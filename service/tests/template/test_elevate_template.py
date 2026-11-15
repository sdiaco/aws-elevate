"""What each role of infra/elevate.yaml can and cannot do, and the guarantees the
design puts on the template (design §8.2, §8.3, §12.1)."""

import json
from pathlib import Path

import pytest
from cfnlint.decode import decode

from aws_elevate.statemachine import definition

TEMPLATE_PATH = Path(__file__).resolve().parents[3] / "infra" / "elevate.yaml"
TEMPLATE, _errors = decode(str(TEMPLATE_PATH))
RESOURCES = TEMPLATE["Resources"]

SSO_READ_PREFIXES = ("Describe", "Get", "List")


def statements(role: str) -> list[dict]:
    out = []
    for policy in RESOURCES[role]["Properties"]["Policies"]:
        out += policy["PolicyDocument"]["Statement"]
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


# ---------- engine role (design §8.2) ----------


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
            assert action.split(":")[0] in ("sso", "dynamodb", "appconfig", "logs"), action


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
    assert set(roles()) == {"EngineRole", "StateMachineRole", "PrecheckRole"}


# ---------- table (design §8.3, §11.3) ----------


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


# ---------- precheck (design §12.1) ----------


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
    ],
)
def test_lambda_runtime_and_handler(name, handler):
    props = RESOURCES[name]["Properties"]
    assert props["Runtime"] == "python3.12"
    assert props["Handler"] == handler


def test_state_machine_definition_is_the_python_one():
    props = RESOURCES["GrantStateMachine"]["Properties"]
    assert props["Definition"] == definition("${EngineArn}", "${BusName}")
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
