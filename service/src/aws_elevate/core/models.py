"""Domain types shared by api and engine. Enum values are the wire strings of
contract/vocabulary.json (design §3.2, §3.3)."""

from dataclasses import dataclass
from enum import StrEnum


class State(StrEnum):
    PENDING = "pending"
    ACTIVATING = "activating"
    ACTIVE = "active"
    REVOKING = "revoking"
    REVOKE_FAILED = "revoke_failed"
    CLOSED = "closed"


class CloseReason(StrEnum):
    EXPIRED = "expired"
    REVOKED = "revoked"
    REJECTED = "rejected"
    TIMED_OUT = "timed_out"
    CANCELLED = "cancelled"
    INVALIDATED = "invalidated"
    FAILED = "failed"


class Approval(StrEnum):
    AUTO = "auto"
    MANUAL = "manual"


class Role(StrEnum):
    REQUESTER = "requester"
    APPROVER = "approver"
    ADMIN = "admin"
    AUDITOR = "auditor"


@dataclass(frozen=True)
class NamedRef:
    """An Identity Center group or an AWS account: id plus display name."""

    id: str
    name: str


@dataclass(frozen=True)
class PermissionSetRef:
    arn: str
    name: str


@dataclass(frozen=True)
class Principal:
    user_id: str
    user_name: str


@dataclass(frozen=True)
class Rule:
    id: str
    group: NamedRef
    permission_set: PermissionSetRef
    accounts: tuple[NamedRef, ...]
    default_seconds: int
    max_seconds: int
    approval: Approval
    approver_group: NamedRef | None
    privileged: bool


@dataclass(frozen=True)
class Grant:
    id: str
    state: State
    rule_id: str
    config_version: int
    requester: Principal
    approver_group: NamedRef | None
    source_ps: PermissionSetRef
    account: str
    duration_seconds: int
    reason: str
    privileged: bool
    created_at: str
    close_reason: CloseReason | None = None
    approver: Principal | None = None
    activated_at: str | None = None
    expires_at: str | None = None
    closed_at: str | None = None
    revoked_by: Principal | None = None
    revoke_reason: str | None = None
    task_token: str | None = None
    error: str | None = None


@dataclass(frozen=True)
class Identity:
    """The caller, as resolved by api from the request signature."""

    user_id: str
    user_name: str
    group_ids: frozenset[str]
    is_admin: bool
    is_auditor: bool


@dataclass(frozen=True)
class SourcePs:
    """Full content of the permission set a rule points to (design §5.1)."""

    arn: str
    name: str
    managed: tuple[str, ...]
    customer_managed: tuple[dict, ...]
    boundary: dict | None
    inline: dict | None
