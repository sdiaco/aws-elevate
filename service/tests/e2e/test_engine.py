"""Engine and state machine against the real organization (plan T1.8).
Grants are written to the table directly: the api arrives in stage 2."""

import time

import harness

from aws_elevate.core.ids import ps_name
from aws_elevate.core.times import parse_iso


def watch(creds: dict, until: str, timeout_s: float = 120) -> float:
    """Probe until the outcome stops being 'allowed'; return seconds waited."""
    started = time.monotonic()
    while True:
        outcome = harness.probe(creds)
        if outcome != "allowed":
            return time.monotonic() - started
        if time.monotonic() - started > timeout_s:
            raise AssertionError(f"still allowed after {timeout_s}s ({until})")
        time.sleep(1)


def active_with_credentials(org, duration_seconds):
    grant = org.new_grant(duration_seconds)
    execution = org.start(grant)
    snap = org.wait_state(grant.id, "active")
    creds = harness.role_credentials(org.human, ps_name(grant.id))
    started = time.monotonic()
    while harness.probe(creds) != "allowed":  # first call may be denied for 1-2 s (measured)
        assert time.monotonic() - started < 30, "never allowed"
        time.sleep(1)
    return grant, execution, snap, creds


def test_expiry_denies_cleans_up_and_emits_events(org):
    grant, execution, snap, creds = active_with_credentials(org, 120)
    arn = org.permission_set(ps_name(grant.id))
    assert arn is not None
    tags = org.tags(arn)
    assert tags["jit"] == "true" and tags["jit:grantId"] == grant.id
    assert tags["jit:approver"] == "auto"

    expires = parse_iso(snap["expiresAt"]).timestamp()
    watch(creds, "expiry", timeout_s=expires - time.time() + 30)
    harness.log(f"denied {time.time() - expires:+.1f}s from the expiry")
    assert time.time() - expires < 10

    org.wait_execution(execution, "SUCCEEDED", timeout_s=300)
    final = org.snapshot(grant.id)
    assert (final["state"], final["closeReason"]) == ("closed", "expired")
    assert org.permission_set(ps_name(grant.id)) is None
    assert org.events_sent(execution) == ["EvCreated", "EvActivated", "EvClosed"]


def test_early_revocation_denies_open_sessions(org):
    grant, execution, _snap, creds = active_with_credentials(org, 900)
    item = org.raw(grant.id)
    # What api does on revoke: active -> revoking with who and why, then wake the wait.
    org.write(
        [
            {
                "Update": {
                    "TableName": org.table,
                    "Key": {"pk": item["pk"], "sk": item["sk"]},
                    "UpdateExpression": "SET #s = :r, revokedBy = :by, revokeReason = :why",
                    "ConditionExpression": "#s = :a",
                    "ExpressionAttributeNames": {"#s": "state"},
                    "ExpressionAttributeValues": {
                        ":r": {"S": "revoking"},
                        ":a": {"S": "active"},
                        ":by": item["requester"],
                        ":why": {"S": "e2e early revocation"},
                    },
                }
            }
        ]
    )
    started = time.monotonic()
    org.sfn.send_task_success(taskToken=item["taskToken"]["S"], output="{}")
    waited = watch(creds, "early revocation", timeout_s=90)
    harness.log(f"denied {time.monotonic() - started:.1f}s after the revocation ({waited:.1f}s)")

    org.wait_execution(execution, "SUCCEEDED", timeout_s=300)
    final = org.snapshot(grant.id)
    assert (final["state"], final["closeReason"]) == ("closed", "revoked")
    assert final["revokeReason"] == "e2e early revocation"
    assert org.permission_set(ps_name(grant.id)) is None
