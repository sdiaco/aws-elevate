import json

import pytest

from aws_elevate.api import handler as module
from aws_elevate.api.config_cache import ConfigUnavailable
from aws_elevate.api.errors import ApiError, forbidden
from aws_elevate.api.handler import Api
from aws_elevate.version import INFRA_VERSION, MIN_CLIENT


class FakeResolver:
    def __init__(self, identity=None, error=None):
        self.identity = identity
        self.error = error

    def resolve(self, event):
        if self.error:
            raise self.error
        return self.identity


class RecordingGrants:
    """Records which route method was called with what."""

    def __init__(self, result=None, error=None):
        self.calls = []
        self.result = result if result is not None else {"grant": {"id": "abcde23456"}}
        self.error = error

    def __getattr__(self, name):
        def call(*args):
            self.calls.append((name, *args[1:]))
            if self.error:
                raise self.error
            return self.result

        return call


class FakeConfig:
    def __init__(self, rules=None, error=None):
        self.rules = rules or []
        self.error = error

    def current(self):
        if self.error:
            raise self.error
        return 3, self.rules


def event(method, proxy, body=None, query=None, version=INFRA_VERSION, headers=None):
    hdrs = {"X-Elevate-Client-Version": version} if version else {}
    hdrs.update(headers or {})
    return {
        "httpMethod": method,
        "pathParameters": {"proxy": proxy} if proxy is not None else None,
        "queryStringParameters": query,
        "headers": hdrs,
        "body": json.dumps(body) if body is not None else None,
        "requestContext": {"requestId": "req-1"},
    }


@pytest.fixture
def api(fx):
    def build(identity=None, grants=None, config=None, resolver=None):
        return Api(
            resolver or FakeResolver(identity or fx.REQUESTER),
            grants or RecordingGrants(),
            config or FakeConfig([fx.AUTO_RULE, fx.MANUAL_RULE]),
        )

    return build


def body_of(resp):
    return json.loads(resp["body"])


# ---------- routing ----------


@pytest.mark.parametrize(
    ("method", "proxy", "body", "query", "call"),
    [
        ("POST", "grants", {"ruleId": "r"}, None, ("create", {"ruleId": "r"})),
        ("GET", "grants", None, {"scope": "all"}, ("list", {"scope": "all"})),
        ("GET", "grants", None, None, ("list", {})),
        ("GET", "grants/abcde23456", None, None, ("get", "abcde23456")),
        ("POST", "grants/abcde23456/approve", None, None, ("approve", "abcde23456")),
        ("POST", "grants/abcde23456/reject", None, None, ("reject", "abcde23456")),
        ("POST", "grants/abcde23456/cancel", None, None, ("cancel", "abcde23456")),
        ("POST", "grants/abcde23456/redrive", None, None, ("redrive", "abcde23456")),
        (
            "POST",
            "grants/abcde23456/revoke",
            {"reason": "r"},
            None,
            ("revoke", "abcde23456", {"reason": "r"}),
        ),
    ],
)
def test_routes_reach_the_grant_methods(api, method, proxy, body, query, call):
    grants = RecordingGrants()
    resp = api(grants=grants).handle(event(method, proxy, body, query))
    assert resp["statusCode"] == (201 if call[0] == "create" else 200)
    assert grants.calls == [call]
    assert body_of(resp) == {"grant": {"id": "abcde23456"}}


def test_routes_pass_the_resolved_identity(api, fx):
    seen = []

    class Grants:
        def get(self, identity, grant_id):
            seen.append(identity)
            return {}

    api(identity=fx.LEAD, grants=Grants()).handle(event("GET", "grants/abcde23456"))
    assert seen == [fx.LEAD]


@pytest.mark.parametrize(
    ("method", "proxy"),
    [
        ("GET", "nothing"),
        ("DELETE", "grants/abcde23456"),
        ("POST", "grants/abcde23456/explode"),
        ("GET", "grants/NOT-AN-ID"),
        ("GET", "grants/abcde23456/approve"),
        ("GET", None),
    ],
)
def test_unknown_route_is_not_found(api, method, proxy):
    grants = RecordingGrants()
    resp = api(grants=grants).handle(event(method, proxy))
    assert resp["statusCode"] == 404
    assert body_of(resp)["error"] == "not_found"
    assert grants.calls == []


# ---------- versions (design §12.2) ----------


def test_every_response_carries_the_version_headers(api):
    for resp in (
        api().handle(event("GET", "grants")),
        api().handle(event("GET", "nothing")),
    ):
        assert resp["headers"]["X-Elevate-Infra-Version"] == INFRA_VERSION
        assert resp["headers"]["X-Elevate-Min-Client"] == MIN_CLIENT
        assert resp["headers"]["Content-Type"] == "application/json"


def test_client_below_the_minimum_gets_426_before_anything_else(api, monkeypatch):
    monkeypatch.setattr(module, "MIN_CLIENT", "0.2.0")
    grants = RecordingGrants()
    resolver = FakeResolver(error=AssertionError("identity must not be resolved"))
    resp = api(grants=grants, resolver=resolver).handle(event("GET", "grants", version="0.1.9"))
    assert resp["statusCode"] == 426
    assert body_of(resp)["error"] == "client_too_old"
    assert "0.2.0" in body_of(resp)["message"]
    assert grants.calls == []


def test_version_header_is_case_insensitive(api):
    e = event("GET", "grants", version=None, headers={"x-elevate-client-version": "0.0.1"})
    assert api().handle(e)["statusCode"] == 200


@pytest.mark.parametrize("version", [None, "latest"])
def test_missing_or_malformed_client_version_is_invalid(api, version):
    resp = api().handle(event("GET", "grants", version=version))
    assert resp["statusCode"] == 400
    assert body_of(resp)["error"] == "invalid_request"


# ---------- errors in the contract format ----------


def test_refused_identity_is_forbidden(api):
    grants = RecordingGrants()
    resp = api(grants=grants, resolver=FakeResolver(error=forbidden())).handle(
        event("GET", "grants")
    )
    assert resp["statusCode"] == 403
    assert body_of(resp) == {"error": "forbidden", "message": "you are not allowed to do this"}
    assert grants.calls == []


def test_api_error_of_a_route_keeps_status_code_and_message(api):
    grants = RecordingGrants(error=ApiError(409, "not_ready", "retry"))
    resp = api(grants=grants).handle(event("POST", "grants/abcde23456/approve"))
    assert resp["statusCode"] == 409
    assert body_of(resp) == {"error": "not_ready", "message": "retry"}


def test_no_valid_rules_is_503(api):
    grants = RecordingGrants(error=ConfigUnavailable("publish one"))
    resp = api(grants=grants).handle(event("GET", "grants"))
    assert resp["statusCode"] == 503
    assert body_of(resp) == {"error": "config_unavailable", "message": "publish one"}


def test_unexpected_error_is_500_without_details(api, caplog):
    grants = RecordingGrants(error=RuntimeError("secret internals"))
    resp = api(grants=grants).handle(event("GET", "grants"))
    assert resp["statusCode"] == 500
    body = body_of(resp)
    assert body["error"] == "internal"
    assert "secret" not in body["message"]
    assert "req-1" in body["message"]
    assert "secret internals" in caplog.text


@pytest.mark.parametrize("raw", ["{not json", "[1, 2"])
def test_body_that_is_not_json_is_invalid(api, raw):
    e = event("POST", "grants")
    e["body"] = raw
    resp = api().handle(e)
    assert resp["statusCode"] == 400
    assert body_of(resp)["error"] == "invalid_request"


# ---------- GET /me ----------


def test_me_lists_user_roles_and_versions(api, fx):
    resp = api(identity=fx.REQUESTER).handle(event("GET", "me"))
    assert resp["statusCode"] == 200
    assert body_of(resp) == {
        "user": {"userId": "u-1", "userName": "user@example.com"},
        "roles": ["requester"],
        "infraVersion": INFRA_VERSION,
        "minClient": MIN_CLIENT,
    }


def test_me_recognises_approver_admin_and_auditor(api, fx):
    both = fx.dataclasses.replace(fx.LEAD, is_admin=True, is_auditor=True)
    assert body_of(api(identity=both).handle(event("GET", "me")))["roles"] == [
        "approver",
        "admin",
        "auditor",
    ]


def test_me_works_without_valid_rules_so_an_admin_can_publish_the_first(api, fx):
    config = FakeConfig(error=ConfigUnavailable("none"))
    resp = api(identity=fx.ADMIN, config=config).handle(event("GET", "me"))
    assert resp["statusCode"] == 200
    assert body_of(resp)["roles"] == ["admin"]


# ---------- Lambda entry point ----------


def test_handler_wires_the_api_once_from_the_environment(monkeypatch):
    env = {
        "AWS_DEFAULT_REGION": "eu-west-1",
        "TABLE_NAME": "elevate-grants",
        "STATE_MACHINE_ARN": "arn:aws:states:eu-west-1:111111111111:stateMachine:elevate-grant",
        "APPCONFIG_APPLICATION_ID": "app1",
        "APPCONFIG_PROFILE_ID": "prof1",
        "EVENT_BUS_NAME": "elevate-events",
        "IDENTITY_STORE_ID": "d-1234567890",
        "REQUESTER_PS_NAME": "ElevateRequester",
        "ADMIN_PS_NAME": "ElevateAdmin",
        "AUDITOR_GROUP_ID": "",
        "MAX_OPEN_GRANTS_PER_USER": "5",
    }
    for name, value in env.items():
        monkeypatch.setenv(name, value)
    monkeypatch.setattr(module, "_api", None)
    first = module._get_api()
    assert isinstance(first, Api)
    assert module._get_api() is first


def test_handler_answers_through_the_wired_api(monkeypatch, api):
    monkeypatch.setattr(module, "_api", api())
    resp = module.handler(event("GET", "nothing"), None)
    assert resp["statusCode"] == 404
