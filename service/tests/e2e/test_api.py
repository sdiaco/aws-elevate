"""The api in a real organization, called as the client calls it: SigV4 signed with the
test user's ElevateRequester or ElevateAdmin credentials from the access portal."""

import json
import time

import harness
import pytest
from botocore.credentials import Credentials
from harness import MANUAL_RULE_ID, OTHER_RULE_ID, RULE_ID


@pytest.fixture(scope="module")
def requester(org):
    return org.creds("ElevateRequester")


@pytest.fixture(scope="module")
def admin(org):
    return org.creds("ElevateAdmin")


def wait_grant(org, creds, grant_id: str, state: str, timeout_s: float) -> dict:
    started = time.monotonic()
    while True:
        status, body = org.call(creds, "GET", f"grants/{grant_id}")
        assert status == 200, body
        if body["grant"]["state"] == state:
            return body
        if time.monotonic() - started > timeout_s:
            raise AssertionError(f"grant {grant_id} still {body['grant']['state']}, wanted {state}")
        time.sleep(5)


def call_when_ready(org, creds, method: str, path: str, body=None) -> tuple[int, dict]:
    """Like the client: 409 not_ready means the execution has not stored its token yet."""
    deadline = time.monotonic() + 60
    while True:
        status, answer = org.call(creds, method, path, body)
        if (status, answer.get("error")) != (409, "not_ready") or time.monotonic() > deadline:
            return status, answer
        time.sleep(2)


def request_body(org, rule_id: str, seconds: int = 300, reason: str = "aws-elevate e2e") -> dict:
    return {
        "ruleId": rule_id,
        "account": harness.env("TARGET_ACCOUNT"),
        "durationSeconds": seconds,
        "reason": reason,
    }


def test_me_rules_and_what_you_will_get(org, requester, admin):
    status, me = org.call(requester, "GET", "me")
    assert status == 200, me
    assert me["user"]["userName"] == harness.env("TEST_USER")
    # The test user is in the requester group and in the admin group, approver of e2e-manual.
    assert me["roles"] == ["requester", "approver"]
    _, me_admin = org.call(admin, "GET", "me")
    assert "admin" in me_admin["roles"]

    status, rules = org.call(requester, "GET", "rules")
    assert status == 200, rules
    assert {r["id"] for r in rules["rules"]} == {RULE_ID, MANUAL_RULE_ID}
    _, all_rules = org.call(admin, "GET", "rules")
    assert {r["id"] for r in all_rules["rules"]} == {RULE_ID, MANUAL_RULE_ID, OTHER_RULE_ID}

    status, perms = org.call(requester, "GET", f"rules/{RULE_ID}/permissions")
    assert status == 200, perms
    assert perms["inlineSummary"][0]["actions"] == [harness.PROBE_ACTION]

    status, check = org.call(admin, "POST", "rules/check", {"doc": org.rules_doc})
    assert (status, check) == (200, {"valid": True, "errors": []})


def test_auto_request_until_expired(org, requester):
    status, body = org.call(requester, "POST", "grants", request_body(org, RULE_ID))
    assert status == 201, body
    grant_id = body["grant"]["id"]
    active = wait_grant(org, requester, grant_id, "active", timeout_s=180)["grant"]
    assert active["actions"] == ["revoke"]
    creds = harness.role_credentials(org.human, f"JIT-{grant_id}")
    deadline = time.monotonic() + 60
    while harness.probe(creds) != "allowed":
        assert time.monotonic() < deadline, "the granted role never allowed the probe"
        time.sleep(3)
    closed = wait_grant(org, requester, grant_id, "closed", timeout_s=600)
    assert closed["grant"]["closeReason"] == "expired"
    assert [row["state"] for row in closed["timeline"]][0] == "activating"
    assert harness.probe(creds) in ("denied", "invalid")


@pytest.mark.skipif(
    not harness.approval_enabled(),
    reason="approval by a second user is opt-in: E2E_APPROVAL=1 with APPROVER_PROFILE",
)
def test_manual_request_approved_by_another_user_then_revoked(org, requester):
    status, body = org.call(requester, "POST", "grants", request_body(org, MANUAL_RULE_ID))
    assert status == 201, body
    grant_id = body["grant"]["id"]
    approver = org.approver_creds()
    _, pending = org.call(approver, "GET", f"grants/{grant_id}")
    assert pending["grant"]["actions"] == ["approve", "reject"]
    status, body = call_when_ready(org, approver, "POST", f"grants/{grant_id}/approve")
    assert status == 200, body
    assert body["grant"]["approver"]["userName"] == harness.APPROVER_USER
    wait_grant(org, requester, grant_id, "active", timeout_s=180)
    status, body = org.call(
        requester, "POST", f"grants/{grant_id}/revoke", {"reason": "aws-elevate e2e done"}
    )
    assert status == 200, body
    closed = wait_grant(org, requester, grant_id, "closed", timeout_s=180)["grant"]
    assert closed["closeReason"] == "revoked"
    assert closed["revokedBy"]["userName"] == harness.env("TEST_USER")


def test_manual_request_refused_to_its_requester_then_cancelled(org, requester, admin):
    reason = 'e2e "quoted" reason'
    request = request_body(org, MANUAL_RULE_ID, reason=reason)
    status, body = org.call(requester, "POST", "grants", request)
    assert status == 201, body
    grant_id = body["grant"]["id"]
    assert body["grant"]["state"] == "pending"
    mail = org.mail_for(grant_id)
    assert mail["Subject"] == (
        f"aws-elevate: approval needed for {MANUAL_RULE_ID} on {request['account']}"
    )
    assert f"Reason: {reason}\n" in mail["Message"]
    assert "Approve:  aws elevate approve " + grant_id in mail["Message"]
    assert not any(line.startswith('"') for line in mail["Message"].splitlines())
    # Approver group member, but it is their own request, from either role.
    for creds in (requester, admin):
        status, refused = org.call(creds, "POST", f"grants/{grant_id}/approve")
        assert (status, refused["error"]) == (403, "forbidden"), refused
    status, body = call_when_ready(org, requester, "POST", f"grants/{grant_id}/cancel")
    assert status == 200, body
    assert (body["grant"]["state"], body["grant"]["closeReason"]) == ("closed", "cancelled")


def test_rule_of_another_group_is_forbidden(org, requester):
    status, body = org.call(requester, "POST", "grants", request_body(org, OTHER_RULE_ID))
    assert (status, body["error"]) == (403, "forbidden"), body
    status, body = org.call(requester, "GET", f"rules/{OTHER_RULE_ID}/permissions")
    assert (status, body["error"]) == (403, "forbidden"), body


def test_foreign_role_is_refused_by_api_gateway(org):
    # OrganizationAccountAccessRole of the service account: same account, wrong role.
    frozen = org.svc.get_credentials().get_frozen_credentials()
    creds = Credentials(frozen.access_key, frozen.secret_key, frozen.token)
    status, body = org.call(creds, "GET", "me")
    assert status == 403
    # API Gateway's own answer, not ours: the request never reached the api Lambda.
    assert "error" not in body, body


def test_invalid_rules_version_is_ignored_and_reported(org, admin):
    doc = json.loads(json.dumps(org.rules_doc))
    doc["rules"][0]["approval"] = "nope"
    bad = org.publish(doc)
    started = time.monotonic()
    while True:
        # Each read lets api refresh its 30 s cache and see the new version.
        status, rules = org.call(admin, "GET", "rules")
        assert status == 200, rules
        row = org.config_row(bad)
        if row and row["rejectedEventSent"]["BOOL"]:
            break
        assert time.monotonic() - started < 120, f"version {bad} not reported: {row}"
        time.sleep(10)
    assert rules["configVersion"] == org.config_version
    assert row["valid"]["BOOL"] is False
    status, versions = org.call(admin, "GET", "config/versions")
    assert status == 200, versions
    listed = {v["number"]: v for v in versions["versions"]}
    assert listed[bad]["valid"] is False
    assert versions["current"] == org.config_version
