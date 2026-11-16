"""Domain types shared by api and engine. Enum values are the wire strings of
contract/vocabulary.json (design §3.2, §3.3)."""

from dataclasses import dataclass
from enum import StrEnum

# How long a manual request waits for its approvers (design §6.2).
APPROVAL_TIMEOUT_SECONDS = 1800


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
class Group:
    """An Identity Center group."""

    id: str
    name: str


@dataclass(frozen=True)
class Account:
    """An AWS account of the organization."""

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
    group: Group
    permission_set: PermissionSetRef
    accounts: tuple[Account, ...]
    default_seconds: int
    max_seconds: int
    approval: Approval
    approver_group: Group | None
    privileged: bool


@dataclass(frozen=True)
class Grant:
    id: str
    state: State
    rule_id: str
    approval: Approval
    config_version: int
    requester: Principal
    approver_group: Group | None
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
    # ARN of JIT-<id>, recorded as soon as it exists so revocation never has to search.
    ephemeral_ps_arn: str | None = None


@dataclass(frozen=True)
class ConfigVersion:
    """What api recorded about a rules version the first time it saw it (design §7.3)."""

    number: int
    valid: bool
    errors: tuple[str, ...] = ()
    author: str | None = None
    created_at: str | None = None
    # False until CloudTrail has recorded who created the version.
    verified: bool = False
    rejected_event_sent: bool = False


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
