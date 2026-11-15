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
from ..core.models import Grant, State
from ..core.policy import (
    build_inline,
    check_limits,
    ephemeral_tags,
    sanitize_description,
    session_duration,
)
from ..core.rules import coherence_errors, find_rule
from ..core.times import iso, parse_iso
from .errors import ActivationFailed, Retryable

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

    def _activated(self, grant: Grant) -> dict:
        remaining = parse_iso(grant.expires_at) - self._now()
        return {
            "grant": snapshot(grant),
            "waitSeconds": max(0, int(remaining.total_seconds())),
        }
