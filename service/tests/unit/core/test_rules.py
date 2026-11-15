import dataclasses

from aws_elevate.core.models import (
    Account,
    Approval,
    Grant,
    Group,
    PermissionSetRef,
    Principal,
    Rule,
    State,
)
from aws_elevate.core.rules import coherence_errors, find_rule, rule_from_dict

PS_ARN = "arn:aws:sso:::permissionSet/ssoins-1/ps-1"
RULE_DOC = {
    "id": "backend-ssm-prod",
    "group": {"id": "g-1", "name": "backend-devs"},
    "permissionSet": {"arn": PS_ARN, "name": "ssm-write"},
    "accounts": [{"id": "111111111111", "name": "prod"}],
    "duration": {"defaultSeconds": 3600, "maxSeconds": 14400},
    "approval": "manual",
    "approverGroup": {"id": "g-2", "name": "leads"},
    "privileged": True,
}
DOC = {"schemaVersion": 1, "rules": [RULE_DOC]}

RULE = Rule(
    id="backend-ssm-prod",
    group=Group(id="g-1", name="backend-devs"),
    permission_set=PermissionSetRef(arn=PS_ARN, name="ssm-write"),
    accounts=(Account(id="111111111111", name="prod"),),
    default_seconds=3600,
    max_seconds=14400,
    approval=Approval.MANUAL,
    approver_group=Group(id="g-2", name="leads"),
    privileged=True,
)

GRANT = Grant(
    id="abcde23456",
    state=State.ACTIVATING,
    rule_id="backend-ssm-prod",
    approval=Approval.MANUAL,
    config_version=3,
    requester=Principal(user_id="u-1", user_name="user@example.com"),
    approver_group=Group(id="g-2", name="leads"),
    source_ps=PermissionSetRef(arn=PS_ARN, name="ssm-write"),
    account="111111111111",
    duration_seconds=7200,
    reason="incident",
    privileged=True,
    created_at="2026-11-15T09:59:00Z",
)


def test_rule_from_dict():
    assert rule_from_dict(RULE_DOC) == RULE


def test_rule_from_dict_auto_without_approver_group():
    doc = {**RULE_DOC, "approval": "auto", "approverGroup": None, "privileged": False}
    rule = rule_from_dict(doc)
    assert rule.approval == Approval.AUTO
    assert rule.approver_group is None


def test_find_rule():
    assert find_rule(DOC, "backend-ssm-prod") == RULE
    assert find_rule(DOC, "other") is None


def test_coherent_grant_has_no_errors():
    assert coherence_errors(GRANT, RULE) == []


def test_missing_rule_is_an_error():
    errors = coherence_errors(GRANT, None)
    assert errors == ["rule backend-ssm-prod is not in config version 3"]


def test_each_mismatch_is_reported():
    grant = dataclasses.replace(
        GRANT,
        source_ps=PermissionSetRef(arn="arn:aws:sso:::permissionSet/ssoins-1/ps-9", name="x"),
        account="222222222222",
        duration_seconds=20000,
        approval=Approval.AUTO,
        privileged=False,
    )
    errors = coherence_errors(grant, RULE)
    assert len(errors) == 5
    assert any("permission set" in e for e in errors)
    assert any("222222222222" in e for e in errors)
    assert any("20000" in e for e in errors)
    assert any("approval" in e for e in errors)
    assert any("privileged" in e for e in errors)
