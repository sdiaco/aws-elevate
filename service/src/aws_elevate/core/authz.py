"""Who can do what on a grant (design §3.3, §11.2). Approver authority comes from the
approver group recorded on the grant, so it still holds after the rule changes; the
approval itself then finds the request no longer conforming (design §6.2)."""

from .models import Grant, Identity, State


def actions(identity: Identity, grant: Grant) -> set[str]:
    own = identity.user_id == grant.requester.user_id
    approver = grant.approver_group is not None and grant.approver_group.id in identity.group_ids
    if not (own or approver or identity.is_admin or identity.is_auditor):
        return set()
    allowed = {"view"}
    if grant.state == State.PENDING:
        if own:
            allowed.add("cancel")
        elif approver:  # never one's own request, admin or not (design §3.3)
            allowed |= {"approve", "reject"}
    elif grant.state in (State.ACTIVE, State.ACTIVATING):
        if own or approver or identity.is_admin:
            allowed.add("revoke")
    elif grant.state == State.REVOKE_FAILED and identity.is_admin:
        allowed.add("redrive")  # Problems view is admin only (design §11.2)
    return allowed


def can(identity: Identity, action: str, grant: Grant) -> bool:
    return action in actions(identity, grant)
