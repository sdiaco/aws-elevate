"""Every route answers with the keys and types of its example in contract/api/, and accepts
the example request. The Go client decodes the same files with unknown fields refused, so
a key added or renamed on one side fails a test on that side."""

import dataclasses
import json
from pathlib import Path

import pytest

from aws_elevate.api.handler import Api
from aws_elevate.api.routes_config import ConfigRoutes
from aws_elevate.api.routes_grants import GrantRoutes
from aws_elevate.api.routes_rules import RuleRoutes
from aws_elevate.core.models import CloseReason, Principal, State
from aws_elevate.version import INFRA_VERSION

API = Path(__file__).resolve().parents[3] / "contract" / "api"
AUTHOR = "arn:aws:sts::111111111111:assumed-role/AWSReservedSSO_ElevateAdmin_x/user@example.com"


class Resolver:
    def __init__(self, identity):
        self.identity = identity

    def resolve(self, event):
        return self.identity


def scenario(fx, name: str):
    """(caller, grants already in the table) for each example."""
    pending = fx.make_grant()
    active = fx.make_grant(
        state=State.ACTIVE,
        approver=Principal("u-2", "lead@example.com"),
        activated_at="2026-11-15T10:01:00Z",
        expires_at="2026-11-15T11:01:00Z",
    )
    failed = dataclasses.replace(
        active,
        state=State.REVOKE_FAILED,
        revoked_by=Principal("u-3", "admin@example.com"),
        revoke_reason="incident closed",
        error="DeleteAccountAssignment: AccessDenied",
    )
    closed = dataclasses.replace(
        active,
        id="bcdef34567",
        state=State.CLOSED,
        close_reason=CloseReason.EXPIRED,
        created_at="2026-11-15T09:59:30Z",
        closed_at="2026-11-15T11:01:02Z",
    )
    return {
        "me": (fx.ADMIN, ()),
        "rules-list": (fx.REQUESTER, ()),
        "rules-permissions": (fx.REQUESTER, ()),
        "permission-sets-permissions": (fx.ADMIN, ()),
        "rules-check": (fx.ADMIN, ()),
        "grants-create": (fx.REQUESTER, ()),
        "grants-list": (fx.REQUESTER, (active, closed)),
        "grants-get": (fx.REQUESTER, (active,)),
        "grants-approve": (fx.LEAD, (pending,)),
        "grants-reject": (fx.LEAD, (pending,)),
        "grants-cancel": (fx.REQUESTER, (pending,)),
        "grants-revoke": (fx.ADMIN, (active,)),
        "grants-redrive": (fx.ADMIN, (failed,)),
        "config-versions": (fx.AUDITOR, ()),
        "config-version": (fx.AUDITOR, ()),
        "error": (fx.REQUESTER, ()),
    }[name]


def respond(fx, name: str, example: dict) -> tuple[int, dict]:
    identity, grants = scenario(fx, name)
    table = fx.FakeTable(*grants)
    table.rows = {
        g.id: [{"at": g.created_at, "state": "pending", "reason": g.reason}] for g in grants
    }
    config = fx.FakeConfig(
        records=[
            fx.config_version(3, author=AUTHOR, created_at="2026-11-15T09:00:00Z", verified=True),
            fx.config_version(2, valid=False, errors=("schema: approval must be auto or manual",)),
        ],
        docs={3: {"schemaVersion": 1, "rules": []}},
    )
    grant_routes = GrantRoutes(
        table=table,
        config=config,
        sfn=fx.FakeSfn(),
        store=fx.FakeStore({"u-1": frozenset({"g-req"}), "u-2": frozenset({"g-lead"})}),
        max_open=5,
        clock=lambda: fx.NOW,
    )
    api = Api(
        Resolver(identity),
        grant_routes,
        config,
        RuleRoutes(
            config=config,
            sso=fx.FakeSso(),
            reserved_accounts=frozenset({"999999999999"}),
            table=table,
        ),
        ConfigRoutes(config),
        {
            "stackId": "arn:aws:cloudformation:eu-west-1:111111111111:stack/elevate/1",
            "rulesApplicationId": "app1",
            "rulesProfileId": "prof1",
            "alertsTopicArn": "arn:aws:sns:eu-west-1:111111111111:elevate-alerts",
            "approvalsTopicArn": "arn:aws:sns:eu-west-1:111111111111:elevate-approvals",
        },
    )
    request = example["request"]
    resp = api.handle(
        {
            "httpMethod": example["method"],
            "pathParameters": {"proxy": example["path"].removeprefix("/v1/")},
            "queryStringParameters": example["query"],
            "headers": {"X-Elevate-Client-Version": INFRA_VERSION},
            "body": json.dumps(request) if request is not None else None,
            "requestContext": {"requestId": "req-1"},
        }
    )
    return resp["statusCode"], json.loads(resp["body"])


EXAMPLES = sorted(p.stem for p in API.glob("*.json"))


def test_every_route_has_an_example():
    assert EXAMPLES == sorted(
        [
            "me",
            "rules-list",
            "rules-permissions",
            "permission-sets-permissions",
            "rules-check",
            "grants-create",
            "grants-list",
            "grants-get",
            "grants-approve",
            "grants-reject",
            "grants-cancel",
            "grants-revoke",
            "grants-redrive",
            "config-versions",
            "config-version",
            "error",
        ]
    )


@pytest.mark.parametrize("name", EXAMPLES)
def test_example_is_well_formed(name):
    example = json.loads((API / f"{name}.json").read_text())
    assert set(example) == {"method", "path", "query", "request", "status", "response"}
    assert example["path"].startswith("/v1/")


@pytest.mark.parametrize("name", EXAMPLES)
def test_route_answers_like_its_example(fx, name):
    example = json.loads((API / f"{name}.json").read_text())
    status, body = respond(fx, name, example)
    assert status == example["status"], body
    fx.assert_same_shape(example["response"], body)


def test_shape_check_catches_a_missing_key_and_a_wrong_type(fx):
    with pytest.raises(AssertionError):
        fx.assert_same_shape({"a": 1, "b": "x"}, {"a": 1})
    with pytest.raises(AssertionError):
        fx.assert_same_shape({"a": 1}, {"a": "1"})
    with pytest.raises(AssertionError):
        fx.assert_same_shape({"a": [{"b": True}]}, {"a": [{"b": 1}]})
    fx.assert_same_shape({"a": None, "b": [{"c": 1}]}, {"a": "x", "b": []})
