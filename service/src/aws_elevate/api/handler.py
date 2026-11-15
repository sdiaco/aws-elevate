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
# (method, path under /v1, route method on GrantRoutes, takes the body, success status)
_GRANT_ROUTES = [
    ("POST", re.compile(r"grants"), "create", True, 201),
    ("GET", re.compile(r"grants"), "list", False, 200),
    ("GET", re.compile(rf"grants/{_ID}"), "get", False, 200),
    ("POST", re.compile(rf"grants/{_ID}/approve"), "approve", False, 200),
    ("POST", re.compile(rf"grants/{_ID}/reject"), "reject", False, 200),
    ("POST", re.compile(rf"grants/{_ID}/cancel"), "cancel", False, 200),
    ("POST", re.compile(rf"grants/{_ID}/revoke"), "revoke", True, 200),
    ("POST", re.compile(rf"grants/{_ID}/redrive"), "redrive", False, 200),
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
    def __init__(self, identity, grants, config):
        self._identity = identity
        self._grants = grants
        self._config = config

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
        for verb, pattern, name, takes_body, status in _GRANT_ROUTES:
            match = pattern.fullmatch(path)
            if verb != method or not match:
                continue
            args = list(match.groups())
            if name == "list":
                args.append(event.get("queryStringParameters") or {})
            if takes_body:
                args.append(_body(event))
            return status, getattr(self._grants, name)(identity, *args)
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
    from ..adapters.stepfunctions import StepFunctions
    from .config_cache import ConfigCache
    from .identity import IdentityResolver
    from .routes_grants import GrantRoutes

    app, profile = os.environ["APPCONFIG_APPLICATION_ID"], os.environ["APPCONFIG_PROFILE_ID"]
    table = GrantsTable(boto3.client("dynamodb"), os.environ["TABLE_NAME"])
    store = IdentityStore(boto3.client("identitystore"), os.environ["IDENTITY_STORE_ID"])
    config = ConfigCache(
        appconfig=AppConfig(boto3.client("appconfig"), app, profile),
        table=table,
        trail=CloudTrail(boto3.client("cloudtrail"), app, profile),
        events=Events(boto3.client("events"), os.environ["EVENT_BUS_NAME"]),
    )
    identity = IdentityResolver(
        roles=IamRoles(boto3.client("iam")),
        store=store,
        requester_ps=os.environ["REQUESTER_PS_NAME"],
        admin_ps=os.environ["ADMIN_PS_NAME"],
        auditor_group=os.environ["AUDITOR_GROUP_ID"] or None,
    )
    grants = GrantRoutes(
        table=table,
        config=config,
        sfn=StepFunctions(boto3.client("stepfunctions"), os.environ["STATE_MACHINE_ARN"]),
        store=store,
        max_open=int(os.environ["MAX_OPEN_GRANTS_PER_USER"]),
    )
    return Api(identity, grants, config)


def _get_api() -> Api:
    global _api
    if _api is None:
        _api = _build_api()
    return _api


def handler(event, context):
    return _get_api().handle(event)
