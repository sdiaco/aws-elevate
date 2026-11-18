"""Grant snapshot returned by every engine action. The state machine builds the event
detail from it, so it holds only what events may publish."""

from .models import Grant, Group, PermissionSetRef, Principal


def _principal(p: Principal | None) -> dict | None:
    return {"userId": p.user_id, "userName": p.user_name} if p else None


def _group(g: Group | None) -> dict | None:
    return {"id": g.id, "name": g.name} if g else None


def _ps(ps: PermissionSetRef) -> dict:
    return {"arn": ps.arn, "name": ps.name}


def snapshot(grant: Grant) -> dict:
    return {
        "grantId": grant.id,
        "state": grant.state.value,
        "requester": _principal(grant.requester),
        "approver": _principal(grant.approver),
        "approverGroup": _group(grant.approver_group),
        "rule": grant.rule_id,
        "sourcePs": _ps(grant.source_ps),
        "account": grant.account,
        "approval": grant.approval.value,
        "privileged": grant.privileged,
        "startsAt": grant.activated_at,
        "expiresAt": grant.expires_at,
        "configVersion": grant.config_version,
        "reason": grant.reason,
        "closeReason": grant.close_reason.value if grant.close_reason else None,
        "revokedBy": _principal(grant.revoked_by),
        "revokeReason": grant.revoke_reason,
        "rejectReason": grant.reject_reason,
        "error": grant.error,
    }
