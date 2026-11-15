import pytest
from botocore.exceptions import ClientError

from aws_elevate.adapters.sso_admin import SsoOperationFailed, SsoTimeout
from aws_elevate.core.models import CloseReason, Principal, State
from aws_elevate.engine.errors import InvalidState, Retryable

ACTIVE = {
    "state": State.ACTIVE,
    "activated_at": "2026-11-15T10:00:00Z",
    "expires_at": "2026-11-15T11:00:00Z",
}
LEAD = Principal(user_id="u-2", user_name="lead@example.com")


def active(grant, const, **changes):
    return grant(**{**ACTIVE, "ephemeral_ps_arn": const.jit_arn, **changes})


# ---------- store_token ----------


def test_store_token_saves_the_token(build, grant):
    engine, table = build(grant(state=State.PENDING))
    engine.store_token("abcde23456", "tok")
    assert table.get("abcde23456").task_token == "tok"


def test_store_token_on_closed_grant_is_a_no_op(build, grant):
    engine, table = build(grant(state=State.CLOSED, close_reason=CloseReason.CANCELLED))
    result = engine.store_token("abcde23456", "tok")
    assert table.get("abcde23456").task_token is None
    assert result["grant"]["state"] == "closed"


@pytest.mark.parametrize("state", [State.REVOKING, State.ACTIVATING, State.REVOKE_FAILED])
def test_store_token_refuses_a_grant_that_moved_on(build, grant, state):
    # An early revocation that lands before the Active wait stored its token: refusing
    # sends the execution to Revoke at once instead of waiting until expiry.
    engine, table = build(grant(state=state))
    with pytest.raises(InvalidState):
        engine.store_token("abcde23456", "tok")
    assert table.get("abcde23456").task_token is None


def test_store_token_on_active_grant(build, grant, const):
    engine, table = build(active(grant, const))
    engine.store_token("abcde23456", "tok")
    assert table.get("abcde23456").task_token == "tok"


# ---------- resolve_wait ----------


def test_resolve_wait_times_out_a_pending_grant(build, grant):
    engine, table = build(grant(state=State.PENDING))
    result = engine.resolve_wait("abcde23456")
    stored = table.get("abcde23456")
    assert (stored.state, stored.close_reason) == (State.CLOSED, CloseReason.TIMED_OUT)
    assert result["decision"] == "timed_out"
    assert result["grant"]["closeReason"] == "timed_out"


def test_resolve_wait_follows_an_approval_that_won(build, grant):
    engine, table = build(grant(state=State.ACTIVATING, approver=LEAD))
    result = engine.resolve_wait("abcde23456")
    assert result["decision"] == "approved"
    assert table.get("abcde23456").state == State.ACTIVATING


def test_resolve_wait_follows_a_rejection_that_won(build, grant):
    engine, _ = build(grant(state=State.CLOSED, close_reason=CloseReason.REJECTED))
    assert engine.resolve_wait("abcde23456")["decision"] == "rejected"


def test_resolve_wait_on_unexpected_state_fails(build, grant):
    engine, _ = build(grant(state=State.REVOKE_FAILED))
    with pytest.raises(InvalidState):
        engine.resolve_wait("abcde23456")


# ---------- revoke ----------


def test_expiry_revokes_in_design_order_and_closes_as_expired(build, grant, admin, clock, const):
    engine, table = build(active(grant, const))
    clock.advance(3600)
    result = engine.revoke("abcde23456")

    assert admin.names() == ["rewrite_inline", "unassign", "delete"]
    _, arn, account, inline = admin.calls[0]
    assert (arn, account) == (const.jit_arn, "111111111111")
    assert inline["Statement"] == [
        {
            "Sid": "JitExpiry",
            "Effect": "Deny",
            "Action": "*",
            "Resource": "*",
            "Condition": {"DateGreaterThan": {"aws:CurrentTime": "2026-11-15T11:00:00Z"}},
        }
    ]
    assert admin.calls[1] == ("unassign", const.jit_arn, "111111111111", "u-1")
    stored = table.get("abcde23456")
    assert (stored.state, stored.close_reason) == (State.CLOSED, CloseReason.EXPIRED)
    assert result["closeReason"] == "expired"
    assert [c[1:3] for c in table.calls if c[0] == "transition"] == [
        (State.ACTIVE, State.REVOKING),
        (State.REVOKING, State.CLOSED),
    ]


def test_early_revocation_started_by_api_closes_as_revoked(build, grant, const):
    engine, table = build(
        active(grant, const, state=State.REVOKING, revoked_by=LEAD, revoke_reason="done")
    )
    assert engine.revoke("abcde23456")["closeReason"] == "revoked"
    stored = table.get("abcde23456")
    assert stored.revoked_by == LEAD and stored.close_reason == CloseReason.REVOKED


def test_revoke_of_failed_activation_closes_as_failed_and_keeps_error(build, grant, admin, const):
    engine, table = build(
        grant(
            activated_at="2026-11-15T10:00:00Z",
            ephemeral_ps_arn=const.jit_arn,
            error="assignment: policy app-read does not exist",
        )
    )
    assert engine.revoke("abcde23456")["closeReason"] == "failed"
    stored = table.get("abcde23456")
    assert stored.close_reason == CloseReason.FAILED
    assert stored.error == "assignment: policy app-read does not exist"
    assert admin.names() == ["rewrite_inline", "unassign", "delete"]


def test_failed_activation_without_recorded_arn_finds_it_by_name(build, grant, admin, const):
    engine, table = build(grant(), existing=const.jit_arn)
    engine.revoke("abcde23456")
    assert admin.calls[-1] == ("delete", const.jit_arn)
    stored = table.get("abcde23456")
    assert stored.close_reason == CloseReason.FAILED
    assert stored.error == "activation did not complete"


def test_failed_activation_that_never_created_anything(build, grant, admin):
    engine, table = build(grant())
    engine.revoke("abcde23456")
    assert admin.calls == []
    assert table.get("abcde23456").close_reason == CloseReason.FAILED


def test_redrive_from_revoke_failed(build, grant, const):
    engine, table = build(active(grant, const, state=State.REVOKE_FAILED, error="timeout"))
    assert engine.revoke("abcde23456")["closeReason"] == "expired"
    assert table.get("abcde23456").state == State.CLOSED


def test_redrive_that_fails_again_stays_revoke_failed(build, grant, admin, const):
    # While waiting for a redrive the record must say revoke_failed (design §6.5).
    admin.fail["unassign"] = SsoTimeout("unassignment still in progress")
    engine, table = build(active(grant, const, state=State.REVOKE_FAILED, error="timeout"))
    with pytest.raises(Retryable):
        engine.revoke("abcde23456")
    assert table.get("abcde23456").state == State.REVOKE_FAILED


def test_revoke_rerun_on_closed_grant_does_nothing(build, grant, admin, const):
    engine, _ = build(active(grant, const, state=State.CLOSED, close_reason=CloseReason.EXPIRED))
    assert engine.revoke("abcde23456")["closeReason"] == "expired"
    assert admin.calls == []


def test_revoke_when_api_moved_it_to_revoking_meanwhile(build, grant, const):
    engine, table = build(active(grant, const))
    original = table.transition

    def api_wins_first(grant_id, src, dst, **attrs):
        if src == State.ACTIVE:
            table.grants[grant_id] = table.grants[grant_id].__class__(
                **{**table.grants[grant_id].__dict__, "state": State.REVOKING, "revoked_by": LEAD}
            )
        return original(grant_id, src, dst, **attrs)

    table.transition = api_wins_first
    assert engine.revoke("abcde23456")["closeReason"] == "revoked"


@pytest.mark.parametrize(
    "failure",
    [
        SsoTimeout("unassignment still in progress"),
        SsoOperationFailed("unassignment: internal error"),
        ClientError({"Error": {"Code": "ThrottlingException", "Message": "slow"}}, "Op"),
    ],
)
def test_cleanup_failure_is_retryable_and_leaves_revoking(build, grant, admin, const, failure):
    admin.fail["unassign"] = failure
    engine, table = build(active(grant, const))
    with pytest.raises(Retryable):
        engine.revoke("abcde23456")
    assert table.get("abcde23456").state == State.REVOKING


def test_revoke_of_pending_grant_is_refused(build, grant, admin):
    engine, _ = build(grant(state=State.PENDING))
    with pytest.raises(InvalidState):
        engine.revoke("abcde23456")
    assert admin.calls == []


# ---------- mark_revoke_failed ----------


def test_mark_revoke_failed(build, grant, const):
    engine, table = build(active(grant, const, state=State.REVOKING))
    result = engine.mark_revoke_failed("abcde23456")
    stored = table.get("abcde23456")
    assert stored.state == State.REVOKE_FAILED
    assert "revocation failed" in stored.error
    assert result["grant"]["state"] == "revoke_failed"


def test_mark_revoke_failed_keeps_an_existing_error(build, grant, const):
    engine, table = build(active(grant, const, state=State.REVOKING, error="assignment: boom"))
    engine.mark_revoke_failed("abcde23456")
    assert table.get("abcde23456").error == "assignment: boom"


def test_mark_revoke_failed_is_idempotent(build, grant, const):
    engine, table = build(active(grant, const, state=State.REVOKE_FAILED, error="x"))
    engine.mark_revoke_failed("abcde23456")
    assert table.get("abcde23456").state == State.REVOKE_FAILED


def test_close_lost_to_an_unexpected_writer_is_reported(build, grant, const):
    engine, table = build(active(grant, const))
    original = table.transition

    def stuck(grant_id, src, dst, **attrs):
        if dst == State.CLOSED:
            from aws_elevate.adapters.grants_table import StateConflict

            raise StateConflict(grant_id)
        return original(grant_id, src, dst, **attrs)

    table.transition = stuck
    with pytest.raises(InvalidState, match="expected closed"):
        engine.revoke("abcde23456")


def test_mark_revoke_failed_on_active_grant_is_refused(build, grant, const):
    engine, _ = build(active(grant, const))
    with pytest.raises(InvalidState):
        engine.mark_revoke_failed("abcde23456")
