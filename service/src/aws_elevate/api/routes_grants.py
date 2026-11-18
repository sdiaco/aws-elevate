"""Grant routes of api (design §6.1, §6.2, §6.4, §6.5, §11.3).

api writes only the transitions the contract gives it, each conditional on the source
state. After a transition that ends a wait it sends the task token; if the wait is
already over the record still wins, because the state machine reads it next."""

import base64
import binascii
import json
import logging
from collections.abc import Callable
from datetime import UTC, datetime, timedelta

from botocore.exceptions import ClientError

from ..adapters.grants_table import RETENTION_MONTHS, Duplicate, StateConflict
from ..core.authz import actions, can, decisions
from ..core.ids import new_grant_id
from ..core.models import (
    APPROVAL_TIMEOUT_SECONDS,
    Approval,
    CloseReason,
    Grant,
    Identity,
    Principal,
    Rule,
    State,
)
from ..core.rules import InvalidRequest, coherence_errors, requestable, validate_request
from ..core.times import iso, parse_iso
from .errors import ApiError, forbidden

PAGE_SIZE = 50
DEFAULT_PERIOD_DAYS = 30  # design §11.3
# Keeps every grant event well under the 256 KB EventBridge limit (design §6.1, §6.4).
MAX_REASON_CHARS = 500
_SCOPES = ("mine", "approvable", "all")

log = logging.getLogger(__name__)


def _invalid(message: str) -> ApiError:
    return ApiError(400, "invalid_request", message)


def _conflict(message: str) -> ApiError:
    return ApiError(409, "state_conflict", message)


def _text(body: dict, key: str) -> str:
    value = body.get(key)
    if not isinstance(value, str) or not value.strip():
        raise _invalid(f"{key} must be a non-empty string")
    return value.strip()


def _reason(body: dict) -> str:
    reason = _text(body, "reason")
    if len(reason) > MAX_REASON_CHARS:
        raise _invalid(f"reason must be at most {MAX_REASON_CHARS} characters")
    return reason


def _principal(p: Principal | None) -> dict | None:
    return {"userId": p.user_id, "userName": p.user_name} if p else None


def _months(start: str, end: str) -> list[str]:
    """yyyy-mm of every month from end back to start."""
    year, month = int(end[:4]), int(end[5:7])
    months = []
    while f"{year:04d}-{month:02d}" >= start[:7]:
        months.append(f"{year:04d}-{month:02d}")
        year, month = (year, month - 1) if month > 1 else (year - 1, 12)
    return months


def _encode(cursor: dict) -> str:
    return base64.urlsafe_b64encode(json.dumps(cursor).encode()).decode()


def _decode(text: str | None) -> dict | None:
    if text is None:
        return None
    try:
        cursor = json.loads(base64.urlsafe_b64decode(text.encode()))
    except (binascii.Error, ValueError) as err:
        raise _invalid("cursor is not one returned by this API") from err
    if not isinstance(cursor, dict) or set(cursor) != {"m", "k"}:
        raise _invalid("cursor is not one returned by this API")
    return cursor


class GrantRoutes:
    def __init__(
        self,
        table,
        config,
        sfn,
        store,
        max_open: int,
        clock: Callable[[], datetime] | None = None,
    ):
        self._table = table
        self._config = config
        self._sfn = sfn
        self._store = store
        self._max_open = max_open
        self._now = clock or (lambda: datetime.now(UTC))

    # ---------- view ----------

    def _view(self, identity: Identity, grant: Grant, rule_ids: set[str]) -> dict:
        """The grant as the API returns it: never the task token, plus what the caller
        can do on it, so the client decides nothing."""
        g = grant
        return {
            "id": g.id,
            "state": g.state.value,
            "closeReason": g.close_reason.value if g.close_reason else None,
            "ruleId": g.rule_id,
            "approval": g.approval.value,
            "configVersion": g.config_version,
            "requester": _principal(g.requester),
            "approver": _principal(g.approver),
            "approverGroup": (
                {"id": g.approver_group.id, "name": g.approver_group.name}
                if g.approver_group
                else None
            ),
            "sourcePs": {"arn": g.source_ps.arn, "name": g.source_ps.name},
            "account": g.account,
            "accountName": g.account_name,
            "durationSeconds": g.duration_seconds,
            "reason": g.reason,
            "privileged": g.privileged,
            "createdAt": g.created_at,
            "activatedAt": g.activated_at,
            "expiresAt": g.expires_at,
            "closedAt": g.closed_at,
            "revokedBy": _principal(g.revoked_by),
            "revokeReason": g.revoke_reason,
            "rejectReason": g.reject_reason,
            "error": g.error,
            "ruleRemoved": g.rule_id not in rule_ids,
            "starting": g.starting,
            "actions": sorted(actions(identity, g) - {"view"}),
            "approvalExpiresAt": _approval_expires_at(g),
        }

    def _rule_ids(self) -> set[str]:
        return {r.id for r in self._config.current()[1]}

    def _reply(self, identity: Identity, grant_id: str) -> dict:
        return {"grant": self._view(identity, self._table.get(grant_id), self._rule_ids())}

    def _visible(self, identity: Identity, grant_id: str) -> Grant:
        grant = self._table.get(grant_id)
        if grant is None:
            raise ApiError(404, "not_found", f"grant {grant_id} does not exist")
        if not can(identity, "view", grant):
            raise forbidden()
        return grant

    # ---------- POST /grants (design §6.1) ----------

    def create(self, identity: Identity, body) -> dict:
        if not isinstance(body, dict):
            raise _invalid("the request body must be a JSON object")
        rule_id = _text(body, "ruleId")
        account = _text(body, "account")
        reason = _reason(body)
        version, rules = self._config.current()
        rule = next((r for r in rules if r.id == rule_id), None)
        if rule is None:
            raise ApiError(404, "not_found", f"rule {rule_id} does not exist")
        if rule not in requestable(rules, identity):
            raise forbidden(f"you are not in the group that can request rule {rule_id}")
        duration = body.get("durationSeconds", rule.default_seconds)
        if not isinstance(duration, int) or isinstance(duration, bool):
            raise _invalid("durationSeconds must be a whole number of seconds")
        try:
            validate_request(rule, account, duration)
        except InvalidRequest as err:
            raise _invalid(str(err)) from err
        if self._table.count_open(identity.user_id) >= self._max_open:
            raise ApiError(
                429,
                "rate_limited",
                f"you already have {self._max_open} open grants; close one first",
            )
        grant = self._new_grant(identity, rule, version, account, duration, reason)
        try:
            self._table.put_new(grant)
        except Duplicate as err:
            raise ApiError(
                409,
                "duplicate",
                "you already have a grant in progress for this permission set and account",
            ) from err
        try:
            self._sfn.start(grant.id, grant.approval.value)
        except ClientError:
            # Nothing was granted and no event was sent: remove the record so its lock
            # does not block the user, then fail loudly.
            log.exception("execution of grant %s did not start; discarding it", grant.id)
            self._table.discard_new(grant)
            raise
        return {"grant": self._view(identity, grant, {r.id for r in rules})}

    def _new_grant(self, identity, rule: Rule, version, account, duration, reason) -> Grant:
        manual = rule.approval == Approval.MANUAL
        return Grant(
            id=new_grant_id(),
            state=State.PENDING if manual else State.ACTIVATING,
            rule_id=rule.id,
            approval=rule.approval,
            config_version=version,
            requester=Principal(user_id=identity.user_id, user_name=identity.user_name),
            approver_group=rule.approver_group,
            source_ps=rule.permission_set,
            account=account,
            account_name=next((a.name for a in rule.accounts if a.id == account), None),
            duration_seconds=duration,
            reason=reason,
            privileged=rule.privileged,
            created_at=iso(self._now()),
        )

    # ---------- approve, reject, cancel (design §6.2) ----------

    def approve(self, identity: Identity, grant_id: str) -> dict:
        grant = self._pending(identity, "approve", grant_id)
        problems = self._nonconformity(grant)
        if problems:
            self._end_wait(
                grant,
                State.CLOSED,
                "invalidated",
                close_reason=CloseReason.INVALIDATED,
                error="; ".join(problems),
            )
        else:
            self._end_wait(grant, State.ACTIVATING, "approved", approver=_me(identity))
        return self._reply(identity, grant_id)

    def reject(self, identity: Identity, grant_id: str, body) -> dict:
        reason = _reason(body if isinstance(body, dict) else {})
        grant = self._pending(identity, "reject", grant_id)
        self._end_wait(
            grant,
            State.CLOSED,
            "rejected",
            close_reason=CloseReason.REJECTED,
            approver=_me(identity),
            reject_reason=reason,
        )
        return self._reply(identity, grant_id)

    def cancel(self, identity: Identity, grant_id: str) -> dict:
        grant = self._pending(identity, "cancel", grant_id)
        self._end_wait(grant, State.CLOSED, "cancelled", close_reason=CloseReason.CANCELLED)
        return self._reply(identity, grant_id)

    def _pending(self, identity: Identity, action: str, grant_id: str) -> Grant:
        grant = self._visible(identity, grant_id)
        if grant.state != State.PENDING:
            raise _conflict(f"grant {grant_id} is {grant.state}, no longer pending")
        if action not in decisions(identity, grant):
            raise forbidden(f"you cannot {action} grant {grant_id}")
        if grant.starting:
            raise ApiError(409, "not_ready", "the request is still starting; retry in a moment")
        return grant

    def _nonconformity(self, grant: Grant) -> list[str]:
        """Why the request would no longer be accepted under the current rules."""
        _, rules = self._config.current()
        rule = next((r for r in rules if r.id == grant.rule_id), None)
        if rule is None:
            return [f"rule {grant.rule_id} no longer exists"]
        problems = coherence_errors(grant, rule)
        if rule.group.id not in self._store.group_ids(grant.requester.user_id):
            problems.append(f"the requester is no longer in group {rule.group.name}")
        return problems

    def _end_wait(self, grant: Grant, dst: State, decision: str, **attrs) -> None:
        # Without the token the wait would not end until its timeout: let the client
        # retry once the execution has stored it.
        if grant.task_token is None:
            raise ApiError(409, "not_ready", "the grant is still starting; retry in a moment")
        try:
            self._table.transition(grant.id, grant.state, dst, **attrs)
        except StateConflict as err:
            raise _conflict(f"grant {grant.id} changed meanwhile; reload it") from err
        # False means the wait is already over (timed out): expected, and the record
        # decides what the execution does next, so there is nothing to report.
        self._sfn.send_success(grant.task_token, {"decision": decision})

    # ---------- revoke (design §6.4) ----------

    def revoke(self, identity: Identity, grant_id: str, body) -> dict:
        reason = _reason(body if isinstance(body, dict) else {})
        grant = self._visible(identity, grant_id)
        if not can(identity, "revoke", grant):
            if grant.state in (State.ACTIVE, State.ACTIVATING):
                raise forbidden(f"you cannot revoke grant {grant_id}")
            raise _conflict(f"grant {grant_id} is {grant.state}, not active")
        if grant.state == State.ACTIVATING:
            raise ApiError(409, "activating", "the grant is activating; retry in a few seconds")
        # A token recorded while active may still be the approval wait's: sending it
        # is harmless, because the active wait then refuses to start on a revoking grant.
        self._end_wait(
            grant, State.REVOKING, "revoked", revoked_by=_me(identity), revoke_reason=reason
        )
        return self._reply(identity, grant_id)

    # ---------- redrive (design §6.5) ----------

    def redrive(self, identity: Identity, grant_id: str) -> dict:
        grant = self._visible(identity, grant_id)
        if not identity.is_admin:
            raise forbidden("only an admin can redrive a revocation")
        # Only revoke_failed. A grant left revoking because recording revoke_failed itself
        # failed shows in the logs and is resolved by hand (maintainer's choice).
        if not can(identity, "redrive", grant):
            raise _conflict(f"grant {grant_id} is {grant.state}, no failed revocation to redrive")
        if not self._sfn.redrive(grant_id):
            raise _conflict(f"the execution of grant {grant_id} cannot be redriven")
        return self._reply(identity, grant_id)

    # ---------- GET /grants/{id} ----------

    def get(self, identity: Identity, grant_id: str) -> dict:
        grant = self._visible(identity, grant_id)
        return {
            "grant": self._view(identity, grant, self._rule_ids()),
            "timeline": self._table.timeline(grant_id),
        }

    # ---------- GET /grants (design §11.3) ----------

    def list(self, identity: Identity, query: dict) -> dict:
        scope = query.get("scope", "mine")
        if scope not in _SCOPES:
            raise _invalid(f"scope must be one of {', '.join(_SCOPES)}")
        status = self._status(query)
        start, end = self._period(query)
        user = query.get("user")
        cursor = _decode(query.get("cursor"))
        after = cursor["k"] if cursor else None

        if scope == "mine":
            if user is not None:
                raise _invalid("user is not allowed with scope=mine")
            grants, key = self._table.by_user(
                identity.user_id, start, end, PAGE_SIZE, after, status
            )
            nxt = {"m": None, "k": key} if key else None
        elif scope == "approvable":
            if user is not None:
                raise _invalid("user is not allowed with scope=approvable")
            if status == "closed":
                raise _invalid("closed is not allowed with scope=approvable")
            grants, key = self._table.open_grants(PAGE_SIZE, after)
            grants = [g for g in grants if self._approves(identity, g)]
            nxt = {"m": None, "k": key} if key else None
        else:
            if not (identity.is_admin or identity.is_auditor):
                raise forbidden("scope=all is for admins and auditors")
            if user is not None:
                grants, key = self._table.by_user(user, start, end, PAGE_SIZE, after, status)
                nxt = {"m": None, "k": key} if key else None
            elif status == "open":
                grants, key = self._table.open_grants(PAGE_SIZE, after)
                nxt = {"m": None, "k": key} if key else None
            else:
                grants, nxt = self._by_months(start, end, cursor, status)

        rule_ids = self._rule_ids()
        return {
            "items": [
                self._view(identity, g, rule_ids) for g in grants if can(identity, "view", g)
            ],
            "cursor": _encode(nxt) if nxt else None,
        }

    @staticmethod
    def _approves(identity: Identity, grant: Grant) -> bool:
        return grant.approver_group is not None and grant.approver_group.id in identity.group_ids

    def _by_months(self, start: str, end: str, cursor: dict | None, status: str | None):
        """Up to one page across the month partitions of the period, newest first."""
        months = _months(start, end)
        month, key = (cursor["m"], cursor["k"]) if cursor else (months[0], None)
        if month not in months:
            raise _invalid("cursor does not belong to this period")
        i, grants = months.index(month), []
        while True:
            page, key = self._table.by_month(
                months[i], start, end, PAGE_SIZE - len(grants), key, status
            )
            grants += page
            if key is None:
                i += 1
                if i == len(months):
                    return grants, None
            if len(grants) >= PAGE_SIZE:
                return grants, {"m": months[i], "k": key}

    @classmethod
    def _status(cls, query: dict) -> str | None:
        """None for every grant, "open" or "closed" (History) to keep only those."""
        is_open = cls._flag(query, "open")
        closed = cls._flag(query, "closed")
        if is_open and closed:
            raise _invalid("open and closed cannot both be true")
        return "open" if is_open else "closed" if closed else None

    @staticmethod
    def _flag(query: dict, name: str) -> bool:
        value = query.get(name)
        if value in (None, "false"):
            return False
        if value == "true":
            return True
        raise _invalid(f"{name} must be true or false")

    def _period(self, query: dict) -> tuple[str, str]:
        now = self._now()
        try:
            end = parse_iso(query["to"]) if "to" in query else now
            start = (
                parse_iso(query["from"])
                if "from" in query
                else end - timedelta(days=DEFAULT_PERIOD_DAYS)
            )
        except ValueError as err:
            raise _invalid("from and to must look like 2026-01-31T00:00:00Z") from err
        if start > end:
            raise _invalid("from must not be after to")
        start_iso, end_iso = iso(start), iso(end)
        # 13 months are kept, so the period spans at most 14 month partitions.
        if len(_months(start_iso, end_iso)) > RETENTION_MONTHS + 1:
            raise _invalid(f"the history goes back {RETENTION_MONTHS} months")
        return start_iso, end_iso


def _me(identity: Identity) -> Principal:
    return Principal(user_id=identity.user_id, user_name=identity.user_name)


def _approval_expires_at(g: Grant) -> str | None:
    """When a pending request times out. The wait starts with the execution, right after
    creation, so this is within seconds of the real timeout."""
    if g.state != State.PENDING:
        return None
    return iso(parse_iso(g.created_at) + timedelta(seconds=APPROVAL_TIMEOUT_SECONDS))
