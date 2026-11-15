"""DynamoDB table `elevate-grants`: grant records, their timeline and the uniqueness lock.

Every state change is a transaction conditional on the expected source state, written
together with its timeline row, so two writers can never both win a transition."""

import calendar
from collections.abc import Callable
from dataclasses import asdict
from datetime import UTC, datetime

from botocore.exceptions import ClientError

from ..core.lifecycle import can_transition
from ..core.models import (
    Approval,
    CloseReason,
    ConfigVersion,
    Grant,
    Group,
    PermissionSetRef,
    Principal,
    State,
)
from ..core.times import iso

RETENTION_MONTHS = 13  # design §11.3


class Duplicate(Exception):
    """The user already has a grant in progress for this permission set and account."""


class StateConflict(Exception):
    """The grant was not in the expected state (someone else moved it first)."""


# Grant field -> item attribute. Order is the item's attribute order.
_FIELDS = {
    "id": "id",
    "state": "state",
    "close_reason": "closeReason",
    "rule_id": "ruleId",
    "approval": "approval",
    "config_version": "configVersion",
    "requester": "requester",
    "approver": "approver",
    "approver_group": "approverGroup",
    "source_ps": "sourcePs",
    "account": "account",
    "duration_seconds": "durationSeconds",
    "reason": "reason",
    "privileged": "privileged",
    "created_at": "createdAt",
    "activated_at": "activatedAt",
    "expires_at": "expiresAt",
    "closed_at": "closedAt",
    "revoked_by": "revokedBy",
    "revoke_reason": "revokeReason",
    "task_token": "taskToken",
    "error": "error",
    "ephemeral_ps_arn": "ephemeralPsArn",
}
_ATTRS = {v: k for k, v in _FIELDS.items()}
_NESTED = {
    "requester": (Principal, {"user_id": "userId", "user_name": "userName"}),
    "approver": (Principal, {"user_id": "userId", "user_name": "userName"}),
    "revoked_by": (Principal, {"user_id": "userId", "user_name": "userName"}),
    "approver_group": (Group, {"id": "id", "name": "name"}),
    "source_ps": (PermissionSetRef, {"arn": "arn", "name": "name"}),
}
_ENUMS = {"state": State, "close_reason": CloseReason, "approval": Approval}


def _value(field: str, value) -> dict:
    """Python value of a Grant field -> DynamoDB attribute value."""
    if field in _NESTED:
        _, names = _NESTED[field]
        data = asdict(value)
        return {"M": {attr: {"S": data[f]} for f, attr in names.items()}}
    if isinstance(value, bool):
        return {"BOOL": value}
    if isinstance(value, int):
        return {"N": str(value)}
    return {"S": str(value)}


def _parse(field: str, attr: dict):
    if field in _NESTED:
        cls, names = _NESTED[field]
        return cls(**{f: attr["M"][a]["S"] for f, a in names.items()})
    if "BOOL" in attr:
        return attr["BOOL"]
    if "N" in attr:
        return int(attr["N"])
    value = attr["S"]
    return _ENUMS[field](value) if field in _ENUMS else value


def _grant_key(grant_id: str) -> dict:
    return {"pk": {"S": f"GRANT#{grant_id}"}, "sk": {"S": "META"}}


def _lock_key(grant: Grant) -> dict:
    owner = f"{grant.requester.user_id}#{grant.source_ps.arn}#{grant.account}"
    return {"pk": {"S": f"LOCK#{owner}"}, "sk": {"S": "LOCK"}}


def to_item(grant: Grant) -> dict:
    """Record of a grant, with index keys. Fields that are None are left out."""
    item = _grant_key(grant.id)
    for field, attr in _FIELDS.items():
        value = getattr(grant, field)
        if value is not None:
            item[attr] = _value(field, value)
    item["gsi1pk"] = {"S": f"USER#{grant.requester.user_id}"}
    item["gsi1sk"] = {"S": grant.created_at}
    item["gsi2pk"] = {"S": f"MONTH#{grant.created_at[:7]}"}
    item["gsi2sk"] = {"S": f"{grant.created_at}#{grant.id}"}
    if grant.state != State.CLOSED:
        item["gsi3pk"] = {"S": "OPEN"}
        item["gsi3sk"] = {"S": grant.created_at}
    return item


def from_item(item: dict) -> Grant:
    # to_item leaves out None fields, so an absent attribute reads back as None.
    fields = dict.fromkeys(_FIELDS)
    fields.update({_ATTRS[a]: _parse(_ATTRS[a], v) for a, v in item.items() if a in _ATTRS})
    return Grant(**fields)


def timeline_item(grant_id: str, state: State, at: datetime, attrs: dict) -> dict:
    row = {
        "pk": {"S": f"GRANT#{grant_id}"},
        "sk": {"S": f"EV#{iso(at)}#{state.value}"},
        "state": {"S": state.value},
    }
    for field, value in attrs.items():
        if field != "task_token" and value is not None:
            row[_FIELDS[field]] = _value(field, value)
    return row


def _retention_ttl(closed_at: datetime) -> int:
    month0 = closed_at.month - 1 + RETENTION_MONTHS
    year, month = closed_at.year + month0 // 12, month0 % 12 + 1
    day = min(closed_at.day, calendar.monthrange(year, month)[1])
    return int(closed_at.replace(year=year, month=month, day=day).timestamp())


def _config_key(version: int) -> dict:
    return {"pk": {"S": "CONFIG"}, "sk": {"S": f"V#{version:08d}"}}


def _config_item(cv: ConfigVersion) -> dict:
    item = {
        **_config_key(cv.number),
        "version": {"N": str(cv.number)},
        "valid": {"BOOL": cv.valid},
        "errors": {"L": [{"S": e} for e in cv.errors]},
        "verified": {"BOOL": cv.verified},
        "rejectedEventSent": {"BOOL": cv.rejected_event_sent},
    }
    if cv.author is not None:
        item["author"] = {"S": cv.author}
    if cv.created_at is not None:
        item["createdAt"] = {"S": cv.created_at}
    return item


def _config_from_item(item: dict) -> ConfigVersion:
    return ConfigVersion(
        number=int(item["version"]["N"]),
        valid=item["valid"]["BOOL"],
        errors=tuple(e["S"] for e in item["errors"]["L"]),
        author=item.get("author", {}).get("S"),
        created_at=item.get("createdAt", {}).get("S"),
        verified=item["verified"]["BOOL"],
        rejected_event_sent=item["rejectedEventSent"]["BOOL"],
    )


def _condition_failed(err: ClientError) -> bool:
    return err.response["Error"]["Code"] == "ConditionalCheckFailedException"


def _cancellation_codes(err: ClientError) -> list[str] | None:
    if err.response.get("Error", {}).get("Code") != "TransactionCanceledException":
        return None
    return [r.get("Code", "None") for r in err.response.get("CancellationReasons", [])]


class GrantsTable:
    def __init__(self, client, table_name: str, clock: Callable[[], datetime] | None = None):
        self._db = client
        self._table = table_name
        self._now = clock or (lambda: datetime.now(UTC))

    def put_new(self, grant: Grant) -> None:
        """Grant, uniqueness lock and first timeline row in one transaction."""
        items = [
            {
                "Put": {
                    "TableName": self._table,
                    "Item": to_item(grant),
                    "ConditionExpression": "attribute_not_exists(pk)",
                }
            },
            {
                "Put": {
                    "TableName": self._table,
                    "Item": {**_lock_key(grant), "grantId": {"S": grant.id}},
                    "ConditionExpression": "attribute_not_exists(pk)",
                }
            },
            {
                "Put": {
                    "TableName": self._table,
                    "Item": timeline_item(grant.id, grant.state, self._now(), {}),
                }
            },
        ]
        try:
            self._db.transact_write_items(TransactItems=items)
        except ClientError as err:
            codes = _cancellation_codes(err)
            if codes and len(codes) > 1 and codes[1] == "ConditionalCheckFailed":
                raise Duplicate(grant.id) from err
            raise

    def get(self, grant_id: str) -> Grant | None:
        resp = self._db.get_item(
            TableName=self._table, Key=_grant_key(grant_id), ConsistentRead=True
        )
        return from_item(resp["Item"]) if "Item" in resp else None

    def transition(self, grant_id: str, src: State, dst: State, **attrs) -> None:
        """Move a grant from `src` to `dst`, setting the given Grant fields. Raises
        StateConflict if the grant is not in `src`. Closing also sets closedAt and the
        retention TTL, drops the grant from the open index and releases the lock."""
        if not can_transition(src, dst):
            raise ValueError(f"transition {src} -> {dst} is not allowed")
        now = self._now()
        names = {"#state": "state"}
        values = {":dst": {"S": dst.value}, ":src": {"S": src.value}}
        sets = ["#state = :dst"]
        for i, (field, value) in enumerate(attrs.items()):
            attr = _FIELDS[field]
            names[f"#{attr}"] = attr
            values[f":a{i}"] = _value(field, value)
            sets.append(f"#{attr} = :a{i}")
        extra = []
        if dst == State.CLOSED:
            grant = self.get(grant_id)
            if grant is None:
                raise StateConflict(f"grant {grant_id} does not exist")
            names.update({"#closedAt": "closedAt", "#ttl": "ttl"})
            values[":closedAt"] = {"S": iso(now)}
            values[":ttl"] = {"N": str(_retention_ttl(now))}
            sets += ["#closedAt = :closedAt", "#ttl = :ttl"]
            extra = [{"Delete": {"TableName": self._table, "Key": _lock_key(grant)}}]
        expression = "SET " + ", ".join(sets)
        if dst == State.CLOSED:
            # The task token is useless once closed; do not keep it for 13 months.
            expression += " REMOVE gsi3pk, gsi3sk, taskToken"
        items = [
            {
                "Update": {
                    "TableName": self._table,
                    "Key": _grant_key(grant_id),
                    "UpdateExpression": expression,
                    "ConditionExpression": "#state = :src",
                    "ExpressionAttributeNames": names,
                    "ExpressionAttributeValues": values,
                }
            },
            {
                "Put": {
                    "TableName": self._table,
                    "Item": timeline_item(grant_id, dst, now, attrs),
                }
            },
            *extra,
        ]
        try:
            self._db.transact_write_items(TransactItems=items)
        except ClientError as err:
            codes = _cancellation_codes(err)
            if codes and codes[0] == "ConditionalCheckFailed":
                raise StateConflict(f"grant {grant_id} is not {src}") from err
            raise

    def set_attrs(self, grant_id: str, state: State, **attrs) -> None:
        """Record Grant fields without a state change, only while the grant is in
        `state`. Used for progress the engine must not lose on a rerun."""
        names = {"#state": "state"}
        values = {":state": {"S": state.value}}
        sets = []
        for i, (field, value) in enumerate(attrs.items()):
            attr = _FIELDS[field]
            names[f"#{attr}"] = attr
            values[f":a{i}"] = _value(field, value)
            sets.append(f"#{attr} = :a{i}")
        try:
            self._db.update_item(
                TableName=self._table,
                Key=_grant_key(grant_id),
                UpdateExpression="SET " + ", ".join(sets),
                ConditionExpression="#state = :state",
                ExpressionAttributeNames=names,
                ExpressionAttributeValues=values,
            )
        except ClientError as err:
            if err.response["Error"]["Code"] == "ConditionalCheckFailedException":
                raise StateConflict(f"grant {grant_id} is not {state}") from err
            raise

    def set_token(self, grant_id: str, token: str) -> bool:
        """Store the current task token, only while the grant waits (pending or active).
        False otherwise, including when the grant is missing."""
        try:
            self._db.update_item(
                TableName=self._table,
                Key=_grant_key(grant_id),
                UpdateExpression="SET taskToken = :t",
                ConditionExpression="#state IN (:pending, :active)",
                ExpressionAttributeNames={"#state": "state"},
                ExpressionAttributeValues={
                    ":t": {"S": token},
                    ":pending": {"S": State.PENDING.value},
                    ":active": {"S": State.ACTIVE.value},
                },
            )
        except ClientError as err:
            if err.response["Error"]["Code"] == "ConditionalCheckFailedException":
                return False
            raise
        return True

    # ---------- config versions (design §7.1, §7.3) ----------

    def get_config(self, version: int) -> ConfigVersion | None:
        resp = self._db.get_item(
            TableName=self._table, Key=_config_key(version), ConsistentRead=True
        )
        return _config_from_item(resp["Item"]) if "Item" in resp else None

    def put_config(self, cv: ConfigVersion) -> bool:
        """Record a version the first time it is seen. False if another container
        recorded it first; its record wins."""
        try:
            self._db.put_item(
                TableName=self._table,
                Item=_config_item(cv),
                ConditionExpression="attribute_not_exists(pk)",
            )
        except ClientError as err:
            if _condition_failed(err):
                return False
            raise
        return True

    def set_config_author(self, version: int, author: str, created_at: str) -> None:
        self._db.update_item(
            TableName=self._table,
            Key=_config_key(version),
            UpdateExpression="SET author = :a, createdAt = :c, verified = :t",
            ExpressionAttributeValues={
                ":a": {"S": author},
                ":c": {"S": created_at},
                ":t": {"BOOL": True},
            },
        )

    def claim_rejected_event(self, version: int) -> bool:
        """True for exactly one caller: the one that must emit elevate.config.rejected."""
        try:
            self._db.update_item(
                TableName=self._table,
                Key=_config_key(version),
                UpdateExpression="SET rejectedEventSent = :t",
                ConditionExpression="rejectedEventSent = :f",
                ExpressionAttributeValues={":t": {"BOOL": True}, ":f": {"BOOL": False}},
            )
        except ClientError as err:
            if _condition_failed(err):
                return False
            raise
        return True

    def release_rejected_event(self, version: int) -> None:
        """The event could not be sent: let the next read try again."""
        self._db.update_item(
            TableName=self._table,
            Key=_config_key(version),
            UpdateExpression="SET rejectedEventSent = :f",
            ExpressionAttributeValues={":f": {"BOOL": False}},
        )
