import dataclasses

import pytest

from aws_elevate.core.models import (
    Account,
    Approval,
    CloseReason,
    Grant,
    Group,
    Identity,
    PermissionSetRef,
    Principal,
    Rule,
    SourcePs,
    State,
)

PS = PermissionSetRef(arn="arn:aws:sso:::permissionSet/ssoins-1/ps-1", name="ssm-write")


def make_rule(**overrides):
    fields = dict(
        id="backend-ssm-prod",
        group=Group(id="g-1", name="backend-devs"),
        permission_set=PS,
        accounts=(Account(id="111111111111", name="prod"),),
        default_seconds=3600,
        max_seconds=14400,
        approval=Approval.AUTO,
        approver_group=None,
        privileged=False,
    )
    fields.update(overrides)
    return Rule(**fields)


def test_enums_compare_equal_to_their_wire_strings():
    assert State.REVOKE_FAILED == "revoke_failed"
    assert CloseReason.TIMED_OUT == "timed_out"
    assert Approval.MANUAL == "manual"


def test_rule_is_immutable():
    rule = make_rule()
    with pytest.raises(dataclasses.FrozenInstanceError):
        rule.max_seconds = 43200


def test_grant_optional_fields_default_to_none():
    grant = Grant(
        id="abcde23456",
        state=State.PENDING,
        rule_id="backend-ssm-prod",
        approval=Approval.MANUAL,
        config_version=3,
        requester=Principal(user_id="u-1", user_name="user@example.com"),
        approver_group=Group(id="g-2", name="leads"),
        source_ps=PS,
        account="111111111111",
        duration_seconds=3600,
        reason="incident",
        privileged=False,
        created_at="2026-11-15T10:00:00Z",
    )
    assert grant.close_reason is None
    assert grant.expires_at is None
    assert grant.task_token is None
    assert grant.ephemeral_ps_arn is None


def test_identity_and_source_ps_hold_their_fields():
    identity = Identity(
        user_id="u-1",
        user_name="user@example.com",
        group_ids=frozenset({"g-1"}),
        is_admin=False,
        is_auditor=True,
    )
    source = SourcePs(
        arn=PS.arn,
        name=PS.name,
        managed=("arn:aws:iam::aws:policy/ReadOnlyAccess",),
        customer_managed=(),
        boundary=None,
        inline=None,
    )
    assert "g-1" in identity.group_ids
    assert source.inline is None
