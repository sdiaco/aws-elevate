"""Rule routes: what a caller may request, what a rule
grants, and the pre-publication check that only api can run because it reads the PS."""

import copy
import dataclasses
import json
from pathlib import Path

import pytest

from aws_elevate.api.errors import ApiError
from aws_elevate.api.routes_rules import CHECK_CODES, RuleRoutes
from aws_elevate.core.policy import MAX_INLINE_CHARS

VOCABULARY = json.loads(
    (Path(__file__).resolve().parents[4] / "contract" / "vocabulary.json").read_text()
)
VALID_DOC = json.loads(
    (
        Path(__file__).resolve().parents[4] / "contract" / "rules" / "valid-auto-and-manual.json"
    ).read_text()
)
DOC_ARN = VALID_DOC["rules"][0]["permissionSet"]["arn"]


@pytest.fixture
def make(fx):
    def build(
        *sources, config=None, reserved=frozenset({"999999999999", "888888888888"}), grants=()
    ):
        sso = fx.FakeSso(*sources)
        routes = RuleRoutes(
            config=config or fx.FakeConfig(),
            sso=sso,
            reserved_accounts=reserved,
            table=fx.FakeTable(*grants),
        )
        return routes, sso

    return build


def error_of(call) -> ApiError:
    with pytest.raises(ApiError) as caught:
        call()
    return caught.value


# ---------- GET /rules ----------


def test_requester_sees_only_the_rules_of_their_groups(make, fx):
    other = dataclasses.replace(fx.AUTO_RULE, id="other", group=fx.LEADS)
    routes, _ = make(config=fx.FakeConfig(fx.AUTO_RULE, other))
    body = routes.list(fx.REQUESTER, {})
    assert body["configVersion"] == 3
    assert [r["id"] for r in body["rules"]] == ["ssm-dev"]


@pytest.mark.parametrize("who", ["ADMIN", "AUDITOR"])
def test_admin_and_auditor_see_every_rule(make, fx, who):
    routes, _ = make()
    body = routes.list(getattr(fx, who), {})
    assert [r["id"] for r in body["rules"]] == ["ssm-dev", "ssm-prod"]


def test_rules_are_in_the_document_form(make, fx):
    routes, _ = make()
    rule = routes.list(fx.REQUESTER, {})["rules"][1]
    assert rule == {
        "id": "ssm-prod",
        "group": {"id": "g-req", "name": "backend-devs"},
        "permissionSet": {"arn": fx.PS_ARN, "name": "ssm-write"},
        "accounts": [{"id": "222222222222", "name": "prod"}],
        "duration": {"defaultSeconds": 3600, "maxSeconds": 14400},
        "approval": "manual",
        "approverGroup": {"id": "g-lead", "name": "leads"},
        "privileged": True,
    }


@pytest.mark.parametrize("who", ["ADMIN", "AUDITOR"])
def test_requestable_scope_hides_the_rules_admins_and_auditors_cannot_request(make, fx, who):
    routes, _ = make()
    assert routes.list(getattr(fx, who), {"scope": "requestable"})["rules"] == []


def test_requestable_scope_keeps_the_rules_of_the_caller_groups(make, fx):
    other = dataclasses.replace(fx.AUTO_RULE, id="other", group=fx.LEADS)
    routes, _ = make(config=fx.FakeConfig(fx.AUTO_RULE, other))
    admin_requester = dataclasses.replace(fx.ADMIN, group_ids=frozenset({"g-req"}))
    body = routes.list(admin_requester, {"scope": "requestable"})
    assert [r["id"] for r in body["rules"]] == ["ssm-dev"]


def test_requestable_scope_lists_the_accounts_where_the_caller_has_an_open_grant(make, fx):
    # Same permission set and account as an open grant: a request would be a duplicate,
    # whatever rule it came from.
    two = dataclasses.replace(
        fx.AUTO_RULE,
        accounts=(
            fx.Account(id="111111111111", name="dev"),
            fx.Account(id="333333333333", name="qa"),
        ),
    )
    other_ps = dataclasses.replace(
        fx.AUTO_RULE, id="other-ps", permission_set=fx.PermissionSetRef(arn="arn:x:ps-9", name="x")
    )
    grants = (
        fx.make_grant(id="aaaaa23456", state=fx.State.ACTIVE, account="111111111111"),
        fx.make_grant(id="bbbbb23456", state=fx.State.CLOSED, account="333333333333"),
        fx.make_grant(id="ccccc23456", requester=fx.Principal("u-9", "x"), account="333333333333"),
    )
    routes, _ = make(config=fx.FakeConfig(two, other_ps), grants=grants)
    body = routes.list(fx.REQUESTER, {"scope": "requestable"})
    assert body["openGrants"] == [
        {"ruleId": "ssm-dev", "account": "111111111111", "grantId": "aaaaa23456", "state": "active"}
    ]
    assert routes.list(fx.REQUESTER, {})["openGrants"] == []


@pytest.mark.parametrize("query", [{"scope": "all"}, {"scope": ""}, {"other": "x"}])
def test_unknown_rules_query_is_refused(make, fx, query):
    routes, _ = make()
    err = error_of(lambda: routes.list(fx.ADMIN, query))
    assert (err.status, err.code) == (400, "invalid_request")


def test_caller_in_no_rule_gets_an_empty_list(make, fx):
    routes, _ = make()
    assert routes.list(fx.STRANGER, {})["rules"] == []


# ---------- GET /rules/{id}/permissions ("Cosa otterrai") ----------


def test_permissions_of_an_own_rule(make, fx):
    routes, sso = make()
    body = routes.permissions(fx.REQUESTER, "ssm-dev")
    assert sso.reads == [fx.PS_ARN]
    assert body == {
        "managed": ["arn:aws:iam::aws:policy/ReadOnlyAccess"],
        "customerManaged": [{"name": "team-boundary", "path": "/"}],
        "boundary": {"managedPolicyArn": "arn:aws:iam::aws:policy/PowerUserAccess"},
        "inline": fx.source_ps().inline,
        "inlineSummary": [
            {"effect": "Allow", "actions": ["ssm:*"], "resources": ["*"], "conditions": None}
        ],
    }


def test_permissions_with_customer_managed_boundary_and_no_inline(make, fx):
    source = fx.source_ps(
        boundary={"CustomerManagedPolicyReference": {"Name": "limit", "Path": "/team/"}},
        inline=None,
        customer_managed=(),
    )
    routes, _ = make(source)
    body = routes.permissions(fx.REQUESTER, "ssm-dev")
    assert body["boundary"] == {"customerManagedPolicy": {"name": "limit", "path": "/team/"}}
    assert body["inline"] is None
    assert body["inlineSummary"] == []
    assert body["customerManaged"] == []


def test_permissions_without_boundary(make, fx):
    routes, _ = make(fx.source_ps(boundary=None))
    assert routes.permissions(fx.REQUESTER, "ssm-dev")["boundary"] is None


def test_requester_asking_for_a_rule_not_theirs_is_forbidden(make, fx):
    routes, sso = make()
    err = error_of(lambda: routes.permissions(fx.STRANGER, "ssm-dev"))
    assert (err.status, err.code) == (403, "forbidden")
    assert sso.reads == []


def test_approver_of_a_rule_reads_its_permissions_without_being_able_to_request_it(make, fx):
    # The approver must see what they grant.
    routes, sso = make()
    assert routes.permissions(fx.LEAD, "ssm-prod")["managed"]
    assert sso.reads == [fx.PS_ARN]


def test_approver_cannot_read_a_rule_they_do_not_approve(make, fx):
    routes, sso = make()
    err = error_of(lambda: routes.permissions(fx.LEAD, "ssm-dev"))  # auto rule, no approvers
    assert (err.status, err.code) == (403, "forbidden")
    assert sso.reads == []


@pytest.mark.parametrize("who", ["ADMIN", "AUDITOR"])
def test_admin_and_auditor_read_the_permissions_of_any_rule(make, fx, who):
    routes, _ = make()
    assert routes.permissions(getattr(fx, who), "ssm-prod")["managed"]


def test_permissions_of_an_unknown_rule_is_not_found(make, fx):
    routes, _ = make()
    err = error_of(lambda: routes.permissions(fx.ADMIN, "nope"))
    assert (err.status, err.code) == (404, "not_found")


def test_permissions_of_a_deleted_permission_set_is_not_found(make, fx):
    routes, _ = make(fx.source_ps(arn="arn:aws:sso:::permissionSet/ssoins-1/ps-other"))
    err = error_of(lambda: routes.permissions(fx.REQUESTER, "ssm-dev"))
    assert (err.status, err.code) == (404, "not_found")
    assert "ssm-write" in err.message


# ---------- GET /permission-sets/permissions (Rules form) ----------


def test_admin_reads_any_permission_set_by_arn(make, fx):
    # The Rules form shows what a PS contains before any rule uses it.
    routes, sso = make()
    body = routes.ps_permissions(fx.ADMIN, {"arn": fx.PS_ARN})
    assert sso.reads == [fx.PS_ARN]
    assert body == routes.permissions(fx.ADMIN, "ssm-dev")


@pytest.mark.parametrize("who", ["REQUESTER", "LEAD", "AUDITOR", "STRANGER"])
def test_only_an_admin_reads_a_permission_set_by_arn(make, fx, who):
    routes, sso = make()
    err = error_of(lambda: routes.ps_permissions(getattr(fx, who), {"arn": fx.PS_ARN}))
    assert (err.status, err.code) == (403, "forbidden")
    assert sso.reads == []


@pytest.mark.parametrize(
    "query",
    [{}, {"arn": "nope"}, {"arn": "arn:aws:sso:::permissionSet/ssoins-1/ps-1", "x": "1"}],
)
def test_permission_set_query_must_be_one_valid_arn(make, fx, query):
    routes, sso = make()
    err = error_of(lambda: routes.ps_permissions(fx.ADMIN, query))
    assert (err.status, err.code) == (400, "invalid_request")
    assert sso.reads == []


def test_unknown_permission_set_by_arn_is_not_found(make, fx):
    routes, _ = make()
    arn = "arn:aws:sso:::permissionSet/ssoins-1/ps-2"
    err = error_of(lambda: routes.ps_permissions(fx.ADMIN, {"arn": arn}))
    assert (err.status, err.code) == (404, "not_found")
    assert arn in err.message


# ---------- POST /rules/check ----------


def test_check_codes_are_the_vocabulary():
    assert list(CHECK_CODES) == VOCABULARY["checkCodes"]


def test_valid_document(make, fx):
    routes, sso = make(fx.source_ps(arn=DOC_ARN))
    assert routes.check(fx.ADMIN, {"doc": VALID_DOC}) == {"valid": True, "errors": []}
    # Both rules point to the same permission set: read once.
    assert sso.reads == [DOC_ARN]


def test_check_is_admin_only(make, fx):
    routes, sso = make(fx.source_ps(arn=DOC_ARN))
    for who in (fx.REQUESTER, fx.AUDITOR):
        err = error_of(lambda w=who: routes.check(w, {"doc": VALID_DOC}))
        assert (err.status, err.code) == (403, "forbidden")
    assert sso.reads == []


@pytest.mark.parametrize("body", [None, [], {}, {"doc": None, "x": 1}])
def test_check_without_a_doc_is_invalid(make, fx, body):
    routes, _ = make()
    err = error_of(lambda: routes.check(fx.ADMIN, body))
    assert (err.status, err.code) == (400, "invalid_request")


def test_invalid_document_lists_parser_errors_with_the_rule_id(make, fx):
    doc = copy.deepcopy(VALID_DOC)
    doc["rules"][1]["approval"] = "nope"
    del doc["rules"][0]["id"]
    routes, sso = make()
    body = routes.check(fx.ADMIN, {"doc": doc})
    assert body["valid"] is False
    assert {(e["ruleId"], e["code"]) for e in body["errors"]} == {
        (None, "invalid"),
        ("db-admin-prod", "invalid"),
    }
    assert all(e["message"] for e in body["errors"])
    # A document that does not parse is not checked against AWS.
    assert sso.reads == []


def test_document_level_error_has_no_rule_id(make, fx):
    routes, _ = make()
    body = routes.check(fx.ADMIN, {"doc": {"schemaVersion": 2, "rules": []}})
    assert body["errors"] == [
        {"ruleId": None, "field": None, "code": "invalid", "message": "schemaVersion must be 1"}
    ]


def test_parser_error_names_the_field_without_the_document_path(make, fx):
    doc = copy.deepcopy(VALID_DOC)
    doc["rules"][1]["accounts"] = []
    routes, _ = make()
    body = routes.check(fx.ADMIN, {"doc": doc})
    assert body["errors"] == [
        {
            "ruleId": "db-admin-prod",
            "field": "accounts",
            "code": "invalid",
            "message": "choose at least one account",
        }
    ]


def test_permission_set_that_does_not_exist(make, fx):
    routes, _ = make()  # knows only fx.PS_ARN
    body = routes.check(fx.ADMIN, {"doc": VALID_DOC})
    assert body["valid"] is False
    assert [(e["ruleId"], e["code"]) for e in body["errors"]] == [
        ("backend-ssm-prod", "ps_not_found"),
        ("db-admin-prod", "ps_not_found"),
    ]
    assert "ssm-write" in body["errors"][0]["message"]
    assert {e["field"] for e in body["errors"]} == {"permissionSet"}


def test_inline_over_the_limit_once_the_expiry_is_added(make, fx):
    # Fits alone, but not with the expiry statement the grant adds.
    resource = "x" * (MAX_INLINE_CHARS - 100)
    inline = {
        "Version": "2012-10-17",
        "Statement": [{"Effect": "Allow", "Action": "s3:GetObject", "Resource": resource}],
    }
    routes, _ = make(fx.source_ps(arn=DOC_ARN, inline=inline))
    body = routes.check(fx.ADMIN, {"doc": VALID_DOC})
    assert body["valid"] is False
    assert {e["code"] for e in body["errors"]} == {"limits"}
    assert "too many" in body["errors"][0]["message"]


def test_too_many_managed_policies_counts_customer_managed_too(make, fx):
    source = fx.source_ps(
        arn=DOC_ARN,
        managed=tuple(f"arn:aws:iam::aws:policy/P{i}" for i in range(6)),
        customer_managed=tuple({"Name": f"c{i}", "Path": "/"} for i in range(5)),
    )
    routes, _ = make(source)
    body = routes.check(fx.ADMIN, {"doc": VALID_DOC})
    assert body["valid"] is False
    assert "11 managed policies" in body["errors"][0]["message"]


@pytest.mark.parametrize("name", ["JIT-abcde23456", "ElevateAdmin", "ElevateRequester"])
def test_a_reserved_permission_set_cannot_be_a_source(make, fx, name):
    # JIT-* are the copies the service makes and deletes; Elevate* are the service's own.
    routes, _ = make(fx.source_ps(arn=DOC_ARN, name=name))
    body = routes.check(fx.ADMIN, {"doc": VALID_DOC})
    assert body["valid"] is False
    assert {e["code"] for e in body["errors"]} == {"reserved"}
    assert name in body["errors"][0]["message"]


def test_a_reserved_account_cannot_be_a_target(make, fx):
    # The management account and the service account: a grant there could tamper with
    # aws-elevate itself.
    doc = copy.deepcopy(VALID_DOC)
    rule_id = doc["rules"][1]["id"]
    routes, _ = make(fx.source_ps(arn=DOC_ARN), reserved=frozenset({"222222222222"}))
    body = routes.check(fx.ADMIN, {"doc": doc})
    assert body["valid"] is False
    assert [(e["ruleId"], e["code"]) for e in body["errors"]] == [(rule_id, "reserved")]
    assert "account 222222222222" in body["errors"][0]["message"]
