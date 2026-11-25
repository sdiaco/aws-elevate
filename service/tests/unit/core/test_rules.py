import copy
import dataclasses
import json
from pathlib import Path

import pytest

from aws_elevate.core.models import (
    Account,
    Approval,
    Grant,
    Group,
    Identity,
    PermissionSetRef,
    Principal,
    Rule,
    State,
)
from aws_elevate.core.rules import (
    InvalidRequest,
    InvalidRules,
    coherence_errors,
    find_rule,
    parse,
    requestable,
    rule_from_dict,
    rule_to_dict,
    validate_request,
)

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


def test_a_different_approver_group_is_reported():
    # An approver of the old group must not approve under a rule that changed hands.
    grant = dataclasses.replace(GRANT, approver_group=Group(id="g-9", name="others"))
    assert coherence_errors(grant, RULE) == [
        "approver group others differs from the rule approver group leads"
    ]


# ---------- parse ----------


def doc_with(**changes):
    rule = copy.deepcopy(RULE_DOC)
    rule.update(changes)
    return {"schemaVersion": 1, "rules": [rule]}


def errors_of(doc) -> list[str]:
    with pytest.raises(InvalidRules) as caught:
        parse(doc)
    return caught.value.errors


def test_parse_valid_document():
    assert parse(DOC) == [RULE]


def test_parse_empty_document():
    assert parse({"schemaVersion": 1, "rules": []}) == []


@pytest.mark.parametrize(
    "doc", [None, [], "rules", {"rules": []}, {"schemaVersion": 2, "rules": []}]
)
def test_parse_refuses_a_bad_envelope(doc):
    assert errors_of(doc)


def test_parse_refuses_unknown_top_level_field():
    assert errors_of({**DOC, "extra": 1}) == ["unknown field extra"]


@pytest.mark.parametrize(
    ("changes", "fragment"),
    [
        ({"id": ""}, "id"),
        ({"id": 7}, "id"),
        ({"group": {"id": "g-1"}}, "group"),
        ({"group": {"id": "g-1", "name": "x", "extra": 1}}, "group"),
        (
            {"permissionSet": {"arn": "arn:aws:iam::111111111111:role/x", "name": "x"}},
            "permissionSet",
        ),
        ({"accounts": []}, "accounts"),
        ({"accounts": [{"id": "1111", "name": "prod"}]}, "accounts[0].id"),
        (
            {
                "accounts": [
                    {"id": "111111111111", "name": "a"},
                    {"id": "111111111111", "name": "b"},
                ]
            },
            "111111111111",
        ),
        ({"duration": {"defaultSeconds": 299, "maxSeconds": 3600}}, "defaultSeconds"),
        ({"duration": {"defaultSeconds": 3600, "maxSeconds": 43201}}, "maxSeconds"),
        ({"duration": {"defaultSeconds": 3600.5, "maxSeconds": 7200}}, "defaultSeconds"),
        ({"duration": {"defaultSeconds": True, "maxSeconds": 7200}}, "defaultSeconds"),
        ({"duration": {"defaultSeconds": 7200, "maxSeconds": 3600}}, "defaultSeconds"),
        ({"approval": "sometimes"}, "approval"),
        ({"approverGroup": None}, "approverGroup"),
        ({"approval": "auto", "privileged": False}, "approverGroup"),
        ({"approval": "auto", "approverGroup": None, "privileged": True}, "privileged"),
        ({"privileged": "yes"}, "privileged"),
        ({"owner": "someone"}, "owner"),
    ],
)
def test_parse_reports_each_constraint(changes, fragment):
    errors = errors_of(doc_with(**changes))
    assert any(fragment in e for e in errors), errors
    assert all(e.startswith("rules[0]") for e in errors), errors


def test_parse_reports_missing_fields():
    rule = {k: v for k, v in RULE_DOC.items() if k != "duration"}
    errors = errors_of({"schemaVersion": 1, "rules": [rule]})
    assert errors == ["rules[0]: missing field duration"]


def test_parse_refuses_duplicate_rule_ids():
    errors = errors_of({"schemaVersion": 1, "rules": [RULE_DOC, RULE_DOC]})
    assert errors == ["rules[1].id: another rule is already named backend-ssm-prod"]


def problems_of(doc) -> list[tuple]:
    with pytest.raises(InvalidRules) as caught:
        parse(doc)
    return [(p.index, p.field, p.message) for p in caught.value.problems]


@pytest.mark.parametrize(
    ("changes", "problem"),
    [
        ({"id": ""}, ("id", "enter a name")),
        ({"accounts": []}, ("accounts", "choose at least one account")),
        ({"group": {"id": "", "name": ""}}, ("group", "choose who can request")),
        ({"approverGroup": {"id": "", "name": ""}}, ("approverGroup", "choose who approves")),
        (
            {"duration": {"defaultSeconds": 60, "maxSeconds": 3600}},
            (
                "duration.defaultSeconds",
                "must be from 5 minutes to 12 hours (300 to 43200 seconds)",
            ),
        ),
        (
            {"duration": {"defaultSeconds": 7200, "maxSeconds": 3600}},
            ("duration.defaultSeconds", "must not be longer than the maximum duration"),
        ),
        (
            {"approval": "auto", "approverGroup": None, "privileged": True},
            ("privileged", "a privileged rule needs manual approval"),
        ),
    ],
)
def test_parse_says_which_field_is_wrong_in_plain_words(changes, problem):
    assert problems_of(doc_with(**changes)) == [(0, *problem)]


def test_document_problem_has_no_rule_and_no_field():
    assert problems_of({"schemaVersion": 2, "rules": []}) == [
        (None, None, "schemaVersion must be 1")
    ]


def test_parse_collects_errors_of_every_rule():
    bad = {**RULE_DOC, "id": "other", "approval": "nope"}
    errors = errors_of({"schemaVersion": 1, "rules": [bad, {**bad, "id": "third"}]})
    assert [e.split(".")[0].split(":")[0] for e in errors] == ["rules[0]", "rules[1]"]


# ---------- requestable, validate_request ----------


def identity(*groups, admin=False):
    return Identity(
        user_id="u-1",
        user_name="user@example.com",
        group_ids=frozenset(groups),
        is_admin=admin,
        is_auditor=False,
    )


def test_requestable_keeps_rules_of_the_caller_groups():
    other = dataclasses.replace(RULE, id="other", group=Group(id="g-9", name="ops"))
    assert requestable([RULE, other], identity("g-1", "g-5")) == [RULE]


def test_admin_requests_only_through_their_groups():
    assert requestable([RULE], identity("g-5", admin=True)) == []


def test_valid_request():
    validate_request(RULE, "111111111111", 3600)


@pytest.mark.parametrize(
    ("account", "duration", "fragment"),
    [
        ("222222222222", 3600, "222222222222"),
        ("111111111111", 14401, "14401"),
        ("111111111111", 299, "299"),
    ],
)
def test_invalid_request(account, duration, fragment):
    with pytest.raises(InvalidRequest) as caught:
        validate_request(RULE, account, duration)
    assert fragment in str(caught.value)


# ---------- rule_to_dict (GET /rules) ----------

EXAMPLES = Path(__file__).resolve().parents[4] / "contract" / "rules"


@pytest.mark.parametrize("name", ["valid-auto-and-manual.json", "valid-empty.json"])
def test_rule_to_dict_gives_back_the_document_form(name):
    doc = json.loads((EXAMPLES / name).read_text())
    assert [rule_to_dict(r) for r in parse(doc)] == doc["rules"]
