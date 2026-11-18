"""Who can do what on a grant. Approver authority comes from the
approver group recorded on the grant, so it still holds after the rule changes; the
approval itself then finds the request no longer conforming."""

from .models import Grant, Identity, State


def decisions(identity: Identity, grant: Grant) -> set[str]:
    """What the caller may decide on a pending request, ready or still starting."""
    if grant.state != State.PENDING:
        return set()
    if identity.user_id == grant.requester.user_id:
        return {"cancel"}
    if grant.approver_group is not None and grant.approver_group.id in identity.group_ids:
        return {"approve", "reject"}  # never one's own request, admin or not
    return set()


def actions(identity: Identity, grant: Grant) -> set[str]:
    own = identity.user_id == grant.requester.user_id
    approver = grant.approver_group is not None and grant.approver_group.id in identity.group_ids
    if not (own or approver or identity.is_admin or identity.is_auditor):
        return set()
    allowed = {"view"}
    if grant.state == State.PENDING:
        if not grant.starting:  # no decision until the wait can receive it
            allowed |= decisions(identity, grant)
    elif grant.state in (State.ACTIVE, State.ACTIVATING):
        if own or approver or identity.is_admin:
            allowed.add("revoke")
    elif grant.state == State.REVOKE_FAILED and identity.is_admin:
        allowed.add("redrive")  # Problems view is admin only
    return allowed


def can(identity: Identity, action: str, grant: Grant) -> bool:
    return action in actions(identity, grant)
