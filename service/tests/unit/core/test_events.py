from aws_elevate.core.events import snapshot
from aws_elevate.core.models import (
    Approval,
    CloseReason,
    Grant,
    Group,
    PermissionSetRef,
    Principal,
    State,
)

GRANT = Grant(
    id="abcde23456",
    state=State.CLOSED,
    rule_id="backend-ssm-prod",
    approval=Approval.MANUAL,
    config_version=3,
    requester=Principal(user_id="u-1", user_name="user@example.com"),
    approver_group=Group(id="g-2", name="leads"),
    source_ps=PermissionSetRef(arn="arn:aws:sso:::permissionSet/ssoins-1/ps-1", name="ssm-write"),
    account="111111111111",
    duration_seconds=3600,
    reason="incident",
    privileged=False,
    created_at="2026-11-15T09:59:00Z",
    close_reason=CloseReason.REVOKED,
    activated_at="2026-11-15T10:00:00Z",
    expires_at="2026-11-15T11:00:00Z",
    revoked_by=Principal(user_id="u-2", user_name="lead@example.com"),
    revoke_reason="done",
    reject_reason="not needed",
    task_token="secret-token",
)


def test_snapshot_has_the_event_fields():
    assert snapshot(GRANT) == {
        "grantId": "abcde23456",
        "state": "closed",
        "requester": {"userId": "u-1", "userName": "user@example.com"},
        "approver": None,
        "approverGroup": {"id": "g-2", "name": "leads"},
        "rule": "backend-ssm-prod",
        "sourcePs": {"arn": "arn:aws:sso:::permissionSet/ssoins-1/ps-1", "name": "ssm-write"},
        "account": "111111111111",
        "approval": "manual",
        "privileged": False,
        "startsAt": "2026-11-15T10:00:00Z",
        "expiresAt": "2026-11-15T11:00:00Z",
        "configVersion": 3,
        "reason": "incident",
        "closeReason": "revoked",
        "revokedBy": {"userId": "u-2", "userName": "lead@example.com"},
        "revokeReason": "done",
        "rejectReason": "not needed",
        "error": None,
    }


def test_snapshot_never_carries_the_task_token():
    assert "secret-token" not in str(snapshot(GRANT))
