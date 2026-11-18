"""Entry point of the `engine` Lambda. Its only input is the state machine payload:
`action`, `grantId` and, for store_token, `taskToken`. Anything else is refused, so the
engine never acts on data that did not come from the grant record."""

import os
import re

_GRANT_ID = re.compile(r"[a-z2-7]{10}")
# action -> whether it takes the task token
_ACTIONS = {
    "snapshot": False,
    "store_token": True,
    "resolve_wait": False,
    "activate": False,
    "revoke": False,
    "mark_revoke_failed": False,
}
_engine = None


def dispatch(event: dict, engine) -> dict:
    action = event.get("action")
    if action not in _ACTIONS:
        raise ValueError(f"unknown action {action!r}")
    allowed = {"action", "grantId", "taskToken"} if _ACTIONS[action] else {"action", "grantId"}
    if set(event) != allowed:
        raise ValueError(f"action {action} takes exactly {sorted(allowed)}, got {sorted(event)}")
    grant_id = event["grantId"]
    if not isinstance(grant_id, str) or not _GRANT_ID.fullmatch(grant_id):
        raise ValueError("grantId is not a grant id")
    method = getattr(engine, action)
    if _ACTIONS[action]:
        return method(grant_id, event["taskToken"])
    return method(grant_id)


def _build_engine():
    import boto3

    from ..adapters.appconfig import AppConfig
    from ..adapters.grants_table import GrantsTable
    from ..adapters.sso_admin import SsoAdmin
    from ..adapters.sso_read import SsoRead
    from .actions import Engine

    sso = boto3.client("sso-admin")
    reader = SsoRead(sso, os.environ["INSTANCE_ARN"])
    return Engine(
        table=GrantsTable(boto3.client("dynamodb"), os.environ["TABLE_NAME"]),
        rules=AppConfig(
            boto3.client("appconfig"),
            os.environ["APPCONFIG_APPLICATION_ID"],
            os.environ["APPCONFIG_PROFILE_ID"],
        ),
        sso_read=reader,
        sso_admin=SsoAdmin(sso, os.environ["INSTANCE_ARN"], reader),
        reserved_accounts=frozenset(
            {os.environ["MANAGEMENT_ACCOUNT_ID"], os.environ["SERVICE_ACCOUNT_ID"]}
        ),
    )


def handler(event, context):
    global _engine
    if _engine is None:
        _engine = _build_engine()
    return dispatch(event, _engine)
