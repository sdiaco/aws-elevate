"""infra/elevate-guardrails.yaml, the optional SCP stack of the management account.
Only template tests: activating SCPs in the test organization is
the owner's call.

Exemptions are exact role ARNs, or the deployer patterns the installer passes (reserved
AWSReservedSSO_* names, which no one can create): a role named like a service role cannot
bypass the policy, and IAM on role/elevate/* is denied to
everyone else, so no one can make or change such a role."""

import json
from pathlib import Path

from cfnlint.decode import decode

TEMPLATE_PATH = Path(__file__).resolve().parents[3] / "infra" / "elevate-guardrails.yaml"
TEMPLATE, _errors = decode(str(TEMPLATE_PATH))
RESOURCES = TEMPLATE["Resources"]

API_ROLE = {
    "Fn::Sub": "arn:${AWS::Partition}:iam::${ServiceAccountId}:role/elevate/${StackName}-api"
}
DEPLOYERS = {"Ref": "DeployerRoleArns"}


def as_list(value) -> list:
    return value if isinstance(value, list) else [value]


def policy() -> dict:
    (scp,) = [r for r in RESOURCES.values() if r["Type"] == "AWS::Organizations::Policy"]
    return scp


def statements() -> dict[str, dict]:
    return {s["Sid"]: s for s in policy()["Properties"]["Content"]["Statement"]}


def exempt(statement: dict) -> list:
    (key,) = statement["Condition"]["ArnNotLike"]
    assert key == "aws:PrincipalArn"
    return as_list(statement["Condition"]["ArnNotLike"][key])


def actions(statement: dict) -> set[str]:
    return set(as_list(statement["Action"]))


def test_template_decodes_without_errors():
    assert not _errors


def test_one_scp_attached_to_the_service_account_only():
    assert len(RESOURCES) == 1
    props = policy()["Properties"]
    assert props["Type"] == "SERVICE_CONTROL_POLICY"
    assert props["TargetIds"] == [{"Ref": "ServiceAccountId"}]


def test_every_statement_denies_all_but_exempt_principals():
    for sid, s in statements().items():
        assert s["Effect"] == "Deny", sid
        assert set(s["Condition"]) == {"ArnNotLike"}, sid
        assert exempt(s), sid


def runtime_exempt_ok(statement: dict) -> None:
    principals = json.dumps(statement["Condition"])
    assert json.dumps(API_ROLE) in principals and json.dumps(DEPLOYERS) in principals


def test_task_tokens_are_left_only_to_the_api_role_and_the_deployers():
    s = statements()["ProtectTaskTokens"]
    assert actions(s) == {
        "states:SendTaskSuccess",
        "states:SendTaskFailure",
        "states:SendTaskHeartbeat",
    }
    # SendTask* has no resource-level permissions (measured with Resource *).
    assert s["Resource"] == "*"
    runtime_exempt_ok(s)


def test_executions_of_elevate_only_are_left_to_the_api_role_and_the_deployers():
    # Other teams share the account: their state machines stay untouched.
    s = statements()["ProtectExecutions"]
    assert actions(s) == {
        "states:StartExecution",
        "states:StopExecution",
        "states:RedriveExecution",
    }
    states = "arn:${AWS::Partition}:states:*:${ServiceAccountId}"
    assert s["Resource"] == [
        {"Fn::Sub": states + ":stateMachine:${StackName}-*"},
        {"Fn::Sub": states + ":execution:${StackName}-*:*"},
    ]
    runtime_exempt_ok(s)


def test_service_resources_and_roles_are_changed_only_by_the_deployers():
    s = statements()
    lambdas, machine, roles = s["ProtectFunctions"], s["ProtectStateMachine"], s["ProtectRoles"]
    assert {
        "lambda:CreateFunction",
        "lambda:UpdateFunctionCode",
        "lambda:UpdateFunctionConfiguration",
        "lambda:DeleteFunction",
        "lambda:AddPermission",
        "lambda:RemovePermission",
    } <= actions(lambdas)
    assert actions(machine) == {"states:UpdateStateMachine", "states:DeleteStateMachine"}
    assert actions(roles) == {"iam:*"}
    assert roles["Resource"] == {
        "Fn::Sub": "arn:${AWS::Partition}:iam::${ServiceAccountId}:role/elevate/*"
    }
    for statement in (lambdas, machine, roles):
        assert exempt(statement) == [DEPLOYERS]


def test_grants_table_and_backups_are_changed_only_by_the_deployers():
    table = statements()["ProtectGrantsTable"]
    assert actions(table) == {
        "dynamodb:PutResourcePolicy",
        "dynamodb:DeleteResourcePolicy",
        "dynamodb:DeleteTable",
        "dynamodb:UpdateTable",
        "dynamodb:UpdateTimeToLive",
        "dynamodb:UpdateContinuousBackups",
        "dynamodb:RestoreTableFromBackup",
        "dynamodb:StartAwsBackupJob",
    }
    assert table["Resource"] == [
        {
            "Fn::Sub": "arn:${AWS::Partition}:dynamodb:*:${ServiceAccountId}"
            ":table/${StackName}-grants"
        },
        {
            "Fn::Sub": "arn:${AWS::Partition}:dynamodb:*:${ServiceAccountId}"
            ":table/${StackName}-grants/backup/*"
        },
    ]
    assert exempt(table) == [DEPLOYERS]


def test_no_wildcard_exemption_besides_the_deployer_parameter():
    for sid, s in statements().items():
        for principal in exempt(s):
            if principal == DEPLOYERS:
                continue
            assert "*" not in json.dumps(principal).replace("${AWS::Partition}", ""), sid


def test_deployer_parameter_has_no_default():
    params = TEMPLATE["Parameters"]
    assert "Default" not in params["DeployerRoleArns"]
    assert params["ServiceAccountId"]["AllowedPattern"] == "^[0-9]{12}$"
    assert params["StackName"]["Default"] == "elevate"


def test_policy_fits_the_scp_size_limit():
    content = policy()["Properties"]["Content"]
    # 5,120 characters; parameters add about 200 when substituted.
    assert len(json.dumps(content, separators=(",", ":"))) < 4500
