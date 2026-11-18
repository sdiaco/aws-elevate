"""Entry point of the `api` Lambda behind API Gateway (one proxy resource under /v1).

Order of every call: client version (426 if too old), identity from the signature
(design §8.1), then the route. Every answer carries the version headers, and every
error has the contract format {"error": <code>, "message": ...}."""

import json
import logging
import os
import re

from ..core.compat import Compat, check
from ..core.models import Identity, Role
from ..core.rules import requestable
from ..version import INFRA_VERSION, MIN_CLIENT
from .config_cache import ConfigUnavailable
from .errors import ApiError

_ID = r"([a-z2-7]{10})"
# (method, path under /v1, route object, its method, extra argument, success status).
# The extra argument is the query string ("query"), the JSON body ("body") or nothing.
_ROUTES = [
    ("POST", re.compile(r"grants"), "grants", "create", "body", 201),
    ("GET", re.compile(r"grants"), "grants", "list", "query", 200),
    ("GET", re.compile(rf"grants/{_ID}"), "grants", "get", None, 200),
    ("POST", re.compile(rf"grants/{_ID}/approve"), "grants", "approve", None, 200),
    ("POST", re.compile(rf"grants/{_ID}/reject"), "grants", "reject", "body", 200),
    ("POST", re.compile(rf"grants/{_ID}/cancel"), "grants", "cancel", None, 200),
    ("POST", re.compile(rf"grants/{_ID}/revoke"), "grants", "revoke", "body", 200),
    ("POST", re.compile(rf"grants/{_ID}/redrive"), "grants", "redrive", None, 200),
    ("GET", re.compile(r"rules"), "rules", "list", "query", 200),
    ("GET", re.compile(r"rules/([^/]+)/permissions"), "rules", "permissions", None, 200),
    ("GET", re.compile(r"permission-sets/permissions"), "rules", "ps_permissions", "query", 200),
    ("POST", re.compile(r"rules/check"), "rules", "check", "body", 200),
    ("GET", re.compile(r"config/versions"), "versions", "list", None, 200),
    ("GET", re.compile(r"config/versions/([0-9]{1,8})"), "versions", "get", None, 200),
]

log = logging.getLogger(__name__)


def _header(event: dict, name: str) -> str | None:
    for key, value in (event.get("headers") or {}).items():
        if key.lower() == name.lower():
            return value
    return None


def _body(event: dict):
    raw = event.get("body")
    if not raw:
        return None
    try:
        return json.loads(raw)
    except ValueError as err:
        raise ApiError(400, "invalid_request", "the request body is not valid JSON") from err


def _check_client(event: dict) -> None:
    version = _header(event, "X-Elevate-Client-Version")
    if version is None:
        raise ApiError(400, "invalid_request", "missing header X-Elevate-Client-Version")
    try:
        # The client checks the infrastructure against its own minimum; here only ours.
        outcome = check(version, INFRA_VERSION, MIN_CLIENT, INFRA_VERSION)
    except ValueError as err:
        raise ApiError(400, "invalid_request", str(err)) from err
    if outcome == Compat.CLIENT_TOO_OLD:
        raise ApiError(
            426,
            "client_too_old",
            f"client {version} is older than {MIN_CLIENT}, the oldest this service accepts; "
            "upgrade aws-elevate",
        )


class Api:
    def __init__(self, identity, grants, config, rules, versions, installation):
        self._identity = identity
        self._config = config
        # Resource ids the admin client needs to publish rules and run doctor with its own
        # credentials, which cannot list them.
        self._installation = installation
        self._targets = {"grants": grants, "rules": rules, "versions": versions}

    def handle(self, event: dict) -> dict:
        try:
            status, body = self._dispatch(event)
        except ApiError as err:
            status, body = err.status, {"error": err.code, "message": err.message}
        except ConfigUnavailable as err:
            status, body = 503, {"error": "config_unavailable", "message": str(err)}
        except Exception:
            request = (event.get("requestContext") or {}).get("requestId")
            log.exception("unexpected error in request %s", request)
            status = 500
            body = {
                "error": "internal",
                "message": f"unexpected error; the operator can find request {request} "
                "in the api logs",
            }
        return {
            "statusCode": status,
            "headers": {
                "Content-Type": "application/json",
                "X-Elevate-Infra-Version": INFRA_VERSION,
                "X-Elevate-Min-Client": MIN_CLIENT,
            },
            "body": json.dumps(body),
        }

    def _dispatch(self, event: dict) -> tuple[int, dict]:
        _check_client(event)
        identity = self._identity.resolve(event)
        method = event.get("httpMethod")
        path = (event.get("pathParameters") or {}).get("proxy") or ""
        if method == "GET" and path == "me":
            return 200, self._me(identity)
        for verb, pattern, target, name, extra, status in _ROUTES:
            match = pattern.fullmatch(path)
            if verb != method or not match:
                continue
            args = list(match.groups())
            if extra == "query":
                args.append(event.get("queryStringParameters") or {})
            elif extra == "body":
                args.append(_body(event))
            return status, getattr(self._targets[target], name)(identity, *args)
        raise ApiError(404, "not_found", f"no route {method} /v1/{path}")

    def _me(self, identity: Identity) -> dict:
        try:
            _, rules = self._config.current()
        except ConfigUnavailable:
            rules = []  # an admin must still see who they are to publish the first rules
        roles = []
        if requestable(rules, identity):
            roles.append(Role.REQUESTER.value)
        if any(r.approver_group and r.approver_group.id in identity.group_ids for r in rules):
            roles.append(Role.APPROVER.value)
        if identity.is_admin:
            roles.append(Role.ADMIN.value)
        if identity.is_auditor:
            roles.append(Role.AUDITOR.value)
        return {
            "user": {"userId": identity.user_id, "userName": identity.user_name},
            "roles": roles,
            "infraVersion": INFRA_VERSION,
            "minClient": MIN_CLIENT,
            "installation": self._installation if identity.is_admin else None,
        }


_api = None


def _build_api() -> Api:
    import boto3

    from ..adapters.appconfig import AppConfig
    from ..adapters.cloudtrail import CloudTrail
    from ..adapters.events import Events
    from ..adapters.grants_table import GrantsTable
    from ..adapters.iam_roles import IamRoles
    from ..adapters.identity_store import IdentityStore
    from ..adapters.sso_read import SsoRead
    from ..adapters.stepfunctions import StepFunctions
    from .config_cache import ConfigCache
    from .identity import IdentityResolver
    from .routes_config import ConfigRoutes
    from .routes_grants import GrantRoutes
    from .routes_rules import RuleRoutes

    app, profile = os.environ["APPCONFIG_APPLICATION_ID"], os.environ["APPCONFIG_PROFILE_ID"]
    table = GrantsTable(boto3.client("dynamodb"), os.environ["TABLE_NAME"])
    store = IdentityStore(boto3.client("identitystore"), os.environ["IDENTITY_STORE_ID"])
    config = ConfigCache(
        appconfig=AppConfig(boto3.client("appconfig"), app, profile),
        table=table,
        trail=CloudTrail(boto3.client("cloudtrail"), app, profile),
        events=Events(boto3.client("events"), os.environ["EVENT_BUS_NAME"]),
        versions_listed=int(os.environ["CONFIG_VERSIONS_LISTED"]),
    )
    identity = IdentityResolver(
        roles=IamRoles(boto3.client("iam")),
        store=store,
        requester_ps=os.environ["REQUESTER_PS_NAME"],
        admin_ps=os.environ["ADMIN_PS_NAME"],
        auditor_ps=os.environ["AUDITOR_PS_NAME"],
        auditor_group=os.environ["AUDITOR_GROUP_ID"] or None,
    )
    grants = GrantRoutes(
        table=table,
        config=config,
        sfn=StepFunctions(boto3.client("stepfunctions"), os.environ["STATE_MACHINE_ARN"]),
        store=store,
        max_open=int(os.environ["MAX_OPEN_GRANTS_PER_USER"]),
    )
    rules = RuleRoutes(
        config=config,
        sso=SsoRead(boto3.client("sso-admin"), os.environ["SSO_INSTANCE_ARN"]),
        reserved_accounts=frozenset(
            {os.environ["MANAGEMENT_ACCOUNT_ID"], os.environ["SERVICE_ACCOUNT_ID"]}
        ),
        table=table,
    )
    installation = {
        "stackId": os.environ["STACK_ID"],
        "rulesApplicationId": app,
        "rulesProfileId": profile,
        "alertsTopicArn": os.environ["ALERTS_TOPIC_ARN"],
        "approvalsTopicArn": os.environ["APPROVALS_TOPIC_ARN"],
    }
    return Api(identity, grants, config, rules, ConfigRoutes(config), installation)


def _get_api() -> Api:
    global _api
    if _api is None:
        _api = _build_api()
    return _api


def handler(event, context):
    return _get_api().handle(event)
