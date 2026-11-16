"""Release test (design §12.2): install the previous release, open a grant, update the
stack, check that the grant still expires and is cleaned up.

`make e2e-up` installs a new stack at harness.PREVIOUS_INFRA with `aws-elevate install`;
this test runs first and leaves the stack on the current code for the other tests. The
execution started by the previous release finishes with the new engine code (spike C14)."""

import time

import harness
import pytest
from harness import RULE_ID

from aws_elevate.version import INFRA_VERSION


def wait_state(org, creds, grant_id: str, state: str, timeout_s: float) -> dict:
    deadline = time.monotonic() + timeout_s
    while True:
        status, body = org.call(creds, "GET", f"grants/{grant_id}")
        assert status == 200, body
        if body["grant"]["state"] == state:
            return body["grant"]
        assert time.monotonic() < deadline, f"grant {grant_id} still {body['grant']['state']}"
        time.sleep(5)


def test_an_update_keeps_an_open_grant_until_it_expires_and_is_cleaned_up(org):
    requester = org.creds("ElevateRequester")
    status, me = org.call(requester, "GET", "me")
    assert status == 200, me
    if me["infraVersion"] != harness.PREVIOUS_INFRA:
        pytest.skip(
            f"the stack runs {me['infraVersion']}, not the previous release: "
            "make e2e-down e2e-up installs it again"
        )

    body = {
        "ruleId": RULE_ID,
        "account": harness.env("TARGET_ACCOUNT"),
        "durationSeconds": 300,
        "reason": "aws-elevate e2e upgrade",
    }
    status, created = org.call(requester, "POST", "grants", body)
    assert status == 201, created
    grant_id = created["grant"]["id"]
    wait_state(org, requester, grant_id, "active", timeout_s=180)
    assert org.permission_set(f"JIT-{grant_id}")

    harness.upgrade(org.svc)
    _, me = org.call(requester, "GET", "me")
    assert me["infraVersion"] == INFRA_VERSION

    closed = wait_state(org, requester, grant_id, "closed", timeout_s=600)
    assert closed["closeReason"] == "expired"
    assert org.permission_set(f"JIT-{grant_id}") is None
