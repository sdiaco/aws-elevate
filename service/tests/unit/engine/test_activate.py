import dataclasses

import pytest
from botocore.exceptions import ClientError

from aws_elevate.adapters.sso_admin import SsoOperationFailed, SsoTimeout
from aws_elevate.core.models import State
from aws_elevate.engine.errors import ActivationFailed, Retryable
from tests.unit.engine.conftest import SOURCE as ENGINE_SOURCE


def client_error(code):
    return ClientError({"Error": {"Code": code, "Message": code}}, "Op")


def test_activate_creates_configures_assigns_and_records(build, grant, admin, const):
    engine, table = build(grant())
    result = engine.activate("abcde23456")

    stored = table.get("abcde23456")
    assert stored.state == State.ACTIVE
    assert stored.activated_at == "2026-11-15T10:00:00Z"
    assert stored.expires_at == "2026-11-15T11:00:00Z"
    assert stored.ephemeral_ps_arn == const.jit_arn
    assert admin.names() == ["create_ephemeral", "configure", "assign"]
    assert result["waitSeconds"] == 3600
    assert result["grant"]["state"] == "active"
    assert result["grant"]["expiresAt"] == "2026-11-15T11:00:00Z"


def test_activate_builds_the_ephemeral_permission_set_from_the_source(build, grant, admin, const):
    engine, _ = build(grant())
    engine.activate("abcde23456")
    _, name, description, session, tags = admin.calls[0]
    assert name == "JIT-abcde23456"
    assert description == "incident 42"
    assert session == "PT1H"
    assert tags["jit"] == "true" and tags["jit:approver"] == "auto"
    _, arn, inline, source = admin.calls[1]
    assert source == const.source
    assert inline["Statement"][0] == const.source.inline["Statement"][0]
    expiry = inline["Statement"][-1]
    assert expiry["Sid"] == "JitExpiry"
    assert expiry["Condition"]["DateGreaterThan"]["aws:CurrentTime"] == "2026-11-15T11:00:00Z"
    assert admin.calls[2] == ("assign", const.jit_arn, "111111111111", "u-1")


def test_rerun_after_permission_set_created_keeps_start_and_skips_create(
    build, grant, admin, clock, const
):
    engine, table = build(
        grant(activated_at="2026-11-15T10:00:00Z", ephemeral_ps_arn=const.jit_arn)
    )
    clock.advance(30)
    result = engine.activate("abcde23456")
    assert admin.names() == ["configure", "assign"]
    assert table.get("abcde23456").expires_at == "2026-11-15T11:00:00Z"
    assert result["waitSeconds"] == 3570


def test_rerun_when_already_active_does_nothing(build, grant, admin, clock):
    engine, _ = build(
        grant(
            state=State.ACTIVE,
            activated_at="2026-11-15T10:00:00Z",
            expires_at="2026-11-15T11:00:00Z",
        )
    )
    clock.advance(600)
    result = engine.activate("abcde23456")
    assert admin.calls == []
    assert result["waitSeconds"] == 3000


def test_wait_seconds_is_at_least_one(build, grant, clock):
    # Step Functions rejects a zero timeout with an error no Catch can handle.
    engine, _ = build(
        grant(
            state=State.ACTIVE,
            activated_at="2026-11-15T10:00:00Z",
            expires_at="2026-11-15T11:00:00Z",
        )
    )
    clock.advance(7200)
    assert engine.activate("abcde23456")["waitSeconds"] == 1


def assert_failed(table, admin, fragment, created=False):
    stored = table.get("abcde23456")
    assert stored.state == State.ACTIVATING  # revocation closes it as failed
    assert fragment in stored.error
    assert ("create_ephemeral" in admin.names()) == created


def test_source_permission_set_deleted(build, grant, admin):
    engine, table = build(grant(), source=None)
    with pytest.raises(ActivationFailed, match="no longer exists"):
        engine.activate("abcde23456")
    assert_failed(table, admin, "no longer exists")


def test_limit_exceeded_at_grant_time(build, grant, admin, const):
    huge = dataclasses.replace(
        const.source,
        inline={
            "Version": "2012-10-17",
            "Statement": [{"Sid": "x" * 11000, "Effect": "Allow", "Action": "*", "Resource": "*"}],
        },
    )
    engine, table = build(grant(), source=huge)
    with pytest.raises(ActivationFailed, match="limit"):
        engine.activate("abcde23456")
    assert_failed(table, admin, "limit")


def test_rule_removed_from_recorded_version(build, grant, admin):
    from_rules = type("R", (), {"get": lambda self, v: {"schemaVersion": 1, "rules": []}})()
    engine, table = build(grant(), rules=from_rules)
    with pytest.raises(ActivationFailed, match="not in config version 3"):
        engine.activate("abcde23456")
    assert_failed(table, admin, "not in config version 3")


def test_config_version_missing_is_not_retried(build, grant, admin):
    class Missing:
        def get(self, version):
            raise client_error("ResourceNotFoundException")

    engine, table = build(grant(), rules=Missing())
    with pytest.raises(ActivationFailed, match="config version 3"):
        engine.activate("abcde23456")
    assert_failed(table, admin, "config version 3")


def test_assignment_failure_keeps_arn_for_cleanup(build, grant, admin, const):
    admin.fail["assign"] = SsoOperationFailed("assignment: policy app-read does not exist")
    engine, table = build(grant())
    with pytest.raises(ActivationFailed, match="app-read"):
        engine.activate("abcde23456")
    assert_failed(table, admin, "app-read", created=True)
    assert table.get("abcde23456").ephemeral_ps_arn == const.jit_arn


def test_validation_error_from_aws_is_not_retried(build, grant, admin):
    admin.fail["create_ephemeral"] = client_error("ValidationException")
    engine, table = build(grant())
    with pytest.raises(ActivationFailed, match="ValidationException"):
        engine.activate("abcde23456")
    assert_failed(table, admin, "ValidationException", created=True)


@pytest.mark.parametrize(
    "failure", [SsoTimeout("assignment still in progress"), client_error("ThrottlingException")]
)
def test_temporary_failures_are_retryable_and_not_recorded(build, grant, admin, failure):
    admin.fail["assign"] = failure
    engine, table = build(grant())
    with pytest.raises(Retryable):
        engine.activate("abcde23456")
    assert table.get("abcde23456").error is None


def test_activate_refuses_a_grant_in_another_state(build, grant, admin):
    engine, _ = build(grant(state=State.PENDING))
    with pytest.raises(ActivationFailed, match="pending"):
        engine.activate("abcde23456")
    assert admin.calls == []


def test_activate_missing_grant(build):
    engine, _ = build()
    with pytest.raises(ActivationFailed, match="does not exist"):
        engine.activate("abcde23456")


def test_snapshot_reads_the_record(build, grant):
    engine, _ = build(grant())
    assert engine.snapshot("abcde23456")["grant"]["grantId"] == "abcde23456"


def test_reserved_source_is_refused_at_activation(build, grant, admin):
    # A version published without the check still cannot grant a reserved set.
    engine, table = build(grant(), source=dataclasses.replace(ENGINE_SOURCE, name="JIT-zzzzz23456"))
    with pytest.raises(ActivationFailed, match="reserved"):
        engine.activate("abcde23456")
    assert_failed(table, admin, "reserved")


def test_reserved_account_is_refused_at_activation(build, grant, admin):
    # A version published without the check still cannot target the management account or
    # the service account.
    engine, table = build(grant(), reserved=frozenset({"111111111111"}))
    with pytest.raises(ActivationFailed, match="account 111111111111 is reserved"):
        engine.activate("abcde23456")
    assert_failed(table, admin, "reserved")
