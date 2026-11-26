"""Text of the built-in e-mails, composed from EventBridge events."""

import unicodedata
from dataclasses import dataclass

from .models import APPROVAL_TIMEOUT_SECONDS

# SNS subjects are UTF-8 with no line breaks or control characters and fewer than 100
# characters: https://docs.aws.amazon.com/sns/latest/api/API_Publish.html
SUBJECT_MAX = 99


@dataclass(frozen=True)
class Email:
    topic: str  # "approvals" or "alerts"
    subject: str
    body: str


def _one_line(text: str) -> str:
    """A line break or control character would split the line or be refused by SNS."""
    return "".join(" " if unicodedata.category(c).startswith("C") else c for c in str(text))


def _subject(text: str) -> str:
    return _one_line(text)[:SUBJECT_MAX]


def _approval_needed(event: dict) -> Email | None:
    d = event["detail"]
    if d["approval"] != "manual":
        return None
    rule = _one_line(d["rule"])
    account = _one_line(d["account"])
    grant = _one_line(d["grantId"])
    lines = [
        f"{_one_line(d['requester']['userName'])} asks for rule {rule} on account {account}.",
        "",
        f"Reason: {_one_line(d['reason'])}",
        f"Approvers: group {_one_line(d['approverGroup']['name'])}",
    ]
    if d["privileged"]:
        lines.append("Privileged: yes")
    lines += [
        f"Unanswered requests expire after {APPROVAL_TIMEOUT_SECONDS // 60} minutes.",
        "",
        f"Approve:  aws elevate approve {grant}",
        f'Reject:   aws elevate approve {grant} --reject --reason "why"',
        "Or open:  aws elevate ui",
    ]
    return Email(
        "approvals",
        _subject(f"aws-elevate: approval needed for {rule} on {account}"),
        "\n".join(lines) + "\n",
    )


def _cleanup_failing(event: dict) -> Email:
    d = event["detail"]
    grant = _one_line(d["grantId"])
    body = (
        f"Elevate could not remove the access of {_one_line(d['requester']['userName'])} "
        f"to account {_one_line(d['account'])} (rule {_one_line(d['rule'])}). "
        "The access may still work: act now.\n"
        "\n"
        f"Error: {_one_line(d['errorCode'])}: {_one_line(d['error'])}\n"
        f"Time: {_one_line(event['time'])}\n"
        "\n"
        "See it under Problems in: aws elevate ui\n"
    )
    return Email("alerts", _subject(f"aws-elevate: cleanup of grant {grant} is failing"), body)


_BUILDERS = {
    "elevate.request.created": _approval_needed,
    "elevate.grant.error": _cleanup_failing,
}


def compose(event: dict) -> Email | None:
    """The e-mail for a full EventBridge event, or None when the event has none."""
    builder = _BUILDERS.get(event["detail-type"])
    return builder(event) if builder else None
