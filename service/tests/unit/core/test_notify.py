import json
from pathlib import Path

from aws_elevate.core.notify import Email, compose

EVENTS = Path(__file__).resolve().parents[4] / "contract" / "events"


def example(name: str) -> dict:
    return json.loads((EVENTS / f"{name}.json").read_text())["detail"]


def event(detail_type: str, detail: dict, time: str = "2026-11-15T10:00:00Z") -> dict:
    return {"detail-type": detail_type, "time": time, "detail": detail}


def test_manual_request_email_is_the_approved_text():
    email = compose(event("elevate.request.created", example("elevate.request.created")))

    assert email == Email(
        "approvals",
        "aws-elevate: approval needed for ssm-prod on 222222222222",
        "user@example.com asks for rule ssm-prod on account 222222222222.\n"
        "\n"
        "Reason: incident 42\n"
        "Approvers: group leads\n"
        "Privileged: yes\n"
        "Unanswered requests expire after 30 minutes.\n"
        "\n"
        "Approve:  aws elevate approve abcde23456\n"
        'Reject:   aws elevate approve abcde23456 --reject --reason "why"\n'
        "Or open:  aws elevate ui\n",
    )


def test_privileged_line_only_for_privileged_rules():
    detail = {**example("elevate.request.created"), "privileged": False}

    email = compose(event("elevate.request.created", detail))

    assert "Privileged:" not in email.body


def test_automatic_request_sends_nothing():
    detail = {**example("elevate.request.created"), "approval": "auto"}

    assert compose(event("elevate.request.created", detail)) is None


def test_grant_error_email_is_the_approved_text():
    email = compose(
        event("elevate.grant.error", example("elevate.grant.error"), "2026-11-15T11:01:05Z")
    )

    assert email == Email(
        "alerts",
        "aws-elevate: cleanup of grant abcde23456 is failing",
        "Elevate could not remove the access of user@example.com to account 222222222222 "
        "(rule ssm-prod). The access may still work: act now.\n"
        "\n"
        "Error: revoke_failed: DeleteAccountAssignment: AccessDenied\n"
        "Time: 2026-11-15T11:01:05Z\n"
        "\n"
        "See it under Problems in: aws elevate ui\n",
    )


def test_grant_error_email_without_an_error_text_prints_only_the_code():
    for missing in (None, ""):
        detail = {**example("elevate.grant.error"), "error": missing}
        body = compose(event("elevate.grant.error", detail)).body
        assert "Error: revoke_failed\n" in body
    detail = {k: v for k, v in example("elevate.grant.error").items() if k != "error"}
    assert "Error: revoke_failed\n" in compose(event("elevate.grant.error", detail)).body


def test_config_rejected_and_other_events_send_nothing():
    for name in ("elevate.config.rejected", "elevate.grant.closed"):
        assert compose(event(name, example(name))) is None


def test_free_text_stays_on_its_line():
    detail = {**example("elevate.request.created"), "reason": 'say "hi"\nnext\tline\\ ok 🙂'}

    email = compose(event("elevate.request.created", detail))

    assert 'Reason: say "hi" next line\\ ok 🙂\n' in email.body


def test_subject_fits_sns():
    detail = {**example("elevate.request.created"), "rule": "r" * 120}

    email = compose(event("elevate.request.created", detail))

    assert len(email.subject) < 100
    assert "\n" not in email.subject


def test_subject_keeps_unicode():
    detail = {**example("elevate.request.created"), "rule": "règle-prod"}

    email = compose(event("elevate.request.created", detail))

    assert email.subject == "aws-elevate: approval needed for règle-prod on 222222222222"
