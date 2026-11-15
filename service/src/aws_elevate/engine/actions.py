"""Engine actions. Each one starts from the grant record, never from its caller's data,
and tolerates being run again by Step Functions."""

import dataclasses
from collections.abc import Callable
from datetime import UTC, datetime, timedelta

from botocore.exceptions import ClientError

from ..adapters.grants_table import StateConflict
from ..adapters.sso_admin import SsoOperationFailed, SsoTimeout
from ..adapters.sso_read import PsNotFound
from ..core.events import snapshot
from ..core.ids import ps_name
from ..core.models import CloseReason, Grant, State
from ..core.policy import (
    build_inline,
    check_limits,
    ephemeral_tags,
    sanitize_description,
    session_duration,
)
from ..core.rules import coherence_errors, find_rule
from ..core.times import iso, parse_iso
from .errors import ActivationFailed, InvalidState, Retryable

# AWS errors that no retry can fix: the activation fails with AWS's message.
_PERMANENT = {
    "ValidationException",
    "AccessDeniedException",
    "ResourceNotFoundException",
    "ServiceQuotaExceededException",
}


def _code(err: ClientError) -> str:
    return err.response.get("Error", {}).get("Code", "")


class Engine:
    def __init__(
        self,
        table,
        rules,
        sso_read,
        sso_admin,
        clock: Callable[[], datetime] | None = None,
    ):
        self._table = table
        self._rules = rules
        self._read = sso_read
        self._admin = sso_admin
        self._now = clock or (lambda: datetime.now(UTC))

    def _get(self, grant_id: str) -> Grant:
        grant = self._table.get(grant_id)
        if grant is None:
            raise ActivationFailed(f"grant {grant_id} does not exist")
        return grant

    def snapshot(self, grant_id: str) -> dict:
        return {"grant": snapshot(self._get(grant_id))}

    # ---------- waits (design §6.2) ----------

    def store_token(self, grant_id: str, token: str) -> dict:
        """Save the token of the current wait (approval or active). A closed grant needs
        none. Any other state means the grant moved on before the wait began, typically
        an early revocation: refusing sends the execution straight to its Catch."""
        stored = self._table.set_token(grant_id, token)
        grant = self._get(grant_id)
        if not stored and grant.state != State.CLOSED:
            raise InvalidState(f"grant {grant_id} is {grant.state}, no wait to join")
        return {"grant": snapshot(grant)}

    def resolve_wait(self, grant_id: str) -> dict:
        """The approval wait timed out. Whoever moved the record first wins: if an
        approval, rejection or cancellation got there before us, follow it."""
        try:
            self._table.transition(
                grant_id, State.PENDING, State.CLOSED, close_reason=CloseReason.TIMED_OUT
            )
        except StateConflict:
            pass  # someone else decided first; the record below says what
        grant = self._get(grant_id)
        if grant.state == State.CLOSED:
            decision = grant.close_reason.value
        elif grant.state == State.ACTIVATING:
            decision = "approved"
        else:
            raise InvalidState(f"grant {grant_id} is {grant.state} after the approval wait")
        return {"grant": snapshot(grant), "decision": decision}

    # ---------- activation (design §6.3) ----------

    def activate(self, grant_id: str) -> dict:
        grant = self._get(grant_id)
        if grant.state == State.ACTIVE:
            return self._activated(grant)
        if grant.state != State.ACTIVATING:
            raise ActivationFailed(f"grant {grant_id} is {grant.state}, not activating")
        try:
            grant = self._activate(grant)
        except ActivationFailed as err:
            self._record_error(grant, str(err))
            raise
        except (SsoTimeout, ClientError) as err:
            if isinstance(err, ClientError) and _code(err) in _PERMANENT:
                message = f"AWS refused the activation: {err}"
                self._record_error(grant, message)
                raise ActivationFailed(message) from err
            raise Retryable(str(err)) from err
        return self._activated(grant)

    def _activate(self, grant: Grant) -> Grant:
        try:
            doc = self._rules.get(grant.config_version)
        except ClientError as err:
            if _code(err) != "ResourceNotFoundException":
                raise
            raise ActivationFailed(f"config version {grant.config_version} not found") from err
        errors = coherence_errors(grant, find_rule(doc, grant.rule_id))
        if errors:
            raise ActivationFailed("; ".join(errors))
        try:
            source = self._read.read_source(grant.source_ps.arn)
        except PsNotFound as err:
            raise ActivationFailed(
                f"source permission set {grant.source_ps.name} no longer exists"
            ) from err

        # The start is recorded once: a rerun must not move the expiry.
        if grant.activated_at is None:
            grant = self._remember(grant, activated_at=iso(self._now()))
        expires_at = parse_iso(grant.activated_at) + timedelta(seconds=grant.duration_seconds)
        inline = build_inline(source.inline, expires_at)
        limits = check_limits(inline, len(source.managed) + len(source.customer_managed))
        if limits:
            raise ActivationFailed("permission set over the AWS limits: " + "; ".join(limits))

        arn = grant.ephemeral_ps_arn
        if arn is None:
            arn = self._admin.create_ephemeral(
                ps_name(grant.id),
                sanitize_description(grant.reason),
                session_duration(grant.duration_seconds),
                ephemeral_tags(grant),
            )
            grant = self._remember(grant, ephemeral_ps_arn=arn)
        self._admin.configure(arn, inline, source)
        try:
            self._admin.assign(arn, grant.account, grant.requester.user_id)
        except SsoOperationFailed as err:
            raise ActivationFailed(str(err)) from err

        attrs = {"activated_at": grant.activated_at, "expires_at": iso(expires_at)}
        try:
            self._table.transition(grant.id, State.ACTIVATING, State.ACTIVE, **attrs)
        except StateConflict as err:
            raise ActivationFailed(f"grant {grant.id} left activating during activation") from err
        return dataclasses.replace(grant, state=State.ACTIVE, **attrs)

    def _remember(self, grant: Grant, **attrs) -> Grant:
        try:
            self._table.set_attrs(grant.id, State.ACTIVATING, **attrs)
        except StateConflict as err:
            raise ActivationFailed(f"grant {grant.id} left activating during activation") from err
        return dataclasses.replace(grant, **attrs)

    def _record_error(self, grant: Grant, message: str) -> None:
        try:
            self._table.set_attrs(grant.id, State.ACTIVATING, error=message)
        except StateConflict:
            pass  # the grant moved on; the revocation path closes it with its own state

    # ---------- revocation (design §6.4) ----------

    def revoke(self, grant_id: str) -> dict:
        """End access and clean up: expiry, early revocation, failed activation and
        redrive all come here. Each step tolerates having been done already."""
        grant = self._get(grant_id)
        if grant.state == State.CLOSED:
            return self._closed(grant)
        # A redrive leaves revoke_failed only once the cleanup succeeds, so the record
        # never hides a revocation that is still failing (design §6.5).
        redrive = grant.state == State.REVOKE_FAILED
        if not redrive:
            grant = self._enter_revoking(grant)
        arn = grant.ephemeral_ps_arn
        if arn is None and grant.expires_at is None:
            # Activation may have created JIT-<id> without recording it.
            arn = self._read.find_by_name(ps_name(grant.id))
        try:
            if arn is not None:
                # Expiry = now, reprovisioned, so open sessions are denied at once.
                self._admin.rewrite_inline(arn, grant.account, build_inline(None, self._now()))
                self._admin.unassign(arn, grant.account, grant.requester.user_id)
                self._admin.delete(arn)
        except (SsoTimeout, SsoOperationFailed, ClientError) as err:
            raise Retryable(f"revocation of {grant_id}: {err}") from err
        if redrive:
            grant = self._enter_revoking(grant)

        reason, attrs = self._close_reason(grant)
        try:
            self._table.transition(
                grant_id, State.REVOKING, State.CLOSED, close_reason=reason, **attrs
            )
        except StateConflict:
            pass  # closed by an earlier run of this step
        return self._closed(self._get(grant_id))

    def _enter_revoking(self, grant: Grant) -> Grant:
        if grant.state == State.REVOKING:
            return grant
        if grant.state not in (State.ACTIVE, State.ACTIVATING, State.REVOKE_FAILED):
            raise InvalidState(f"grant {grant.id} is {grant.state}, nothing to revoke")
        try:
            self._table.transition(grant.id, grant.state, State.REVOKING)
        except StateConflict:
            pass  # api started an early revocation meanwhile; the record says so
        grant = self._get(grant.id)
        if grant.state != State.REVOKING:
            raise InvalidState(f"grant {grant.id} is {grant.state}, expected revoking")
        return grant

    @staticmethod
    def _close_reason(grant: Grant) -> tuple[CloseReason, dict]:
        if grant.expires_at is None:  # never became active
            return CloseReason.FAILED, {"error": grant.error or "activation did not complete"}
        if grant.revoked_by is not None:
            return CloseReason.REVOKED, {}
        return CloseReason.EXPIRED, {}

    @staticmethod
    def _closed(grant: Grant) -> dict:
        if grant.state != State.CLOSED:
            raise InvalidState(f"grant {grant.id} is {grant.state}, expected closed")
        return {"grant": snapshot(grant), "closeReason": grant.close_reason.value}

    def mark_revoke_failed(self, grant_id: str) -> dict:
        """Revocation kept failing: the grant needs an operator (design §6.5)."""
        grant = self._get(grant_id)
        if grant.state == State.REVOKING:
            error = grant.error or "revocation failed after retries; see the execution history"
            try:
                self._table.transition(grant_id, State.REVOKING, State.REVOKE_FAILED, error=error)
            except StateConflict:
                pass  # an earlier run of this step already marked it
            grant = self._get(grant_id)
        if grant.state != State.REVOKE_FAILED:
            raise InvalidState(f"grant {grant_id} is {grant.state}, expected revoking")
        return {"grant": snapshot(grant)}

    def _activated(self, grant: Grant) -> dict:
        remaining = parse_iso(grant.expires_at) - self._now()
        return {
            "grant": snapshot(grant),
            # At least 1: a zero timeout is a States.Runtime error, which no Catch handles.
            "waitSeconds": max(1, int(remaining.total_seconds())),
        }
