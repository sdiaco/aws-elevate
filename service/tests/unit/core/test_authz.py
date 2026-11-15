"""Who can do what on a grant (design §3.3, §11.2)."""

import pytest

from aws_elevate.core.authz import actions, can
from aws_elevate.core.models import (
    Approval,
    Grant,
    Group,
    Identity,
    PermissionSetRef,
    Principal,
    State,
)

REQUESTER = Principal(user_id="u-1", user_name="dev@example.com")
LEADS = Group(id="g-leads", name="leads")


def grant(state=State.PENDING, approver_group=LEADS, requester=REQUESTER):
    return Grant(
        id="abcde23456",
        state=state,
        rule_id="backend-ssm-prod",
        approval=Approval.MANUAL if approver_group else Approval.AUTO,
        config_version=1,
        requester=requester,
        approver_group=approver_group,
        source_ps=PermissionSetRef(arn="arn:aws:sso:::permissionSet/ssoins-1/ps-1", name="x"),
        account="111111111111",
        duration_seconds=3600,
        reason="incident",
        privileged=False,
        created_at="2026-11-15T10:00:00Z",
    )


def who(user_id="u-9", groups=(), admin=False, auditor=False):
    return Identity(
        user_id=user_id,
        user_name=f"{user_id}@example.com",
        group_ids=frozenset(groups),
        is_admin=admin,
        is_auditor=auditor,
    )


SELF = who("u-1")
LEAD = who("u-2", groups=["g-leads"])
ADMIN = who("u-3", admin=True)
ADMIN_IN_GROUP = who("u-4", groups=["g-leads"], admin=True)
AUDITOR = who("u-5", auditor=True)
STRANGER = who("u-6", groups=["g-other"])


@pytest.mark.parametrize(
    ("identity", "expected"),
    [
        (SELF, {"view", "cancel"}),
        (LEAD, {"view", "approve", "reject"}),
        (ADMIN, {"view"}),
        (ADMIN_IN_GROUP, {"view", "approve", "reject"}),
        (AUDITOR, {"view"}),
        (STRANGER, set()),
    ],
)
def test_pending(identity, expected):
    assert actions(identity, grant()) == expected


def test_approver_who_is_the_requester_cannot_approve():
    lead_requester = Principal(user_id="u-2", user_name="u-2@example.com")
    assert actions(LEAD, grant(requester=lead_requester)) == {"view", "cancel"}


@pytest.mark.parametrize("state", [State.ACTIVE, State.ACTIVATING])
@pytest.mark.parametrize(
    ("identity", "expected"),
    [
        (SELF, {"view", "revoke"}),
        (LEAD, {"view", "revoke"}),
        (ADMIN, {"view", "revoke"}),
        (AUDITOR, {"view"}),
        (STRANGER, set()),
    ],
)
def test_active(state, identity, expected):
    # activating is listed so api can answer 409 activating instead of 403.
    assert actions(identity, grant(state=state)) == expected


def test_auto_grant_has_no_approver():
    assert actions(LEAD, grant(state=State.ACTIVE, approver_group=None)) == set()


@pytest.mark.parametrize(
    ("identity", "expected"),
    [(SELF, {"view"}), (LEAD, {"view"}), (ADMIN, {"view", "redrive"}), (AUDITOR, {"view"})],
)
def test_revoke_failed_only_admin_redrives(identity, expected):
    assert actions(identity, grant(state=State.REVOKE_FAILED)) == expected


@pytest.mark.parametrize("state", [State.REVOKING, State.CLOSED])
def test_nothing_but_view_on_revoking_or_closed(state):
    for identity in (SELF, LEAD, ADMIN, AUDITOR):
        assert actions(identity, grant(state=state)) == {"view"}


def test_can():
    assert can(LEAD, "approve", grant())
    assert not can(AUDITOR, "approve", grant())
