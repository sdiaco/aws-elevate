import json
from datetime import UTC, datetime, timedelta, timezone

import pytest

from aws_elevate.core.policy import (
    build_inline,
    check_limits,
    expiry_statement,
    sanitize_description,
    serialize,
    session_duration,
    summarize,
)

EXPIRES = datetime(2026, 11, 15, 10, 30, 0, tzinfo=UTC)

READ_S3 = {"Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::bucket/*"}


def test_expiry_statement_matches_design_text():
    assert expiry_statement(EXPIRES) == {
        "Sid": "JitExpiry",
        "Effect": "Deny",
        "Action": "*",
        "Resource": "*",
        "Condition": {"DateGreaterThan": {"aws:CurrentTime": "2026-11-15T10:30:00Z"}},
    }


def test_expiry_statement_converts_to_utc():
    rome = timezone(timedelta(hours=1))
    stmt = expiry_statement(datetime(2026, 11, 15, 11, 30, tzinfo=rome))
    assert stmt["Condition"]["DateGreaterThan"]["aws:CurrentTime"] == "2026-11-15T10:30:00Z"


def test_expiry_statement_rejects_naive_datetime():
    with pytest.raises(ValueError):
        expiry_statement(datetime(2026, 11, 15, 10, 30))


def test_build_inline_without_source_has_only_expiry():
    assert build_inline(None, EXPIRES) == {
        "Version": "2012-10-17",
        "Statement": [expiry_statement(EXPIRES)],
    }


def test_build_inline_keeps_source_statements_and_appends_expiry():
    source = {"Version": "2012-10-17", "Statement": [READ_S3]}
    assert build_inline(source, EXPIRES)["Statement"] == [READ_S3, expiry_statement(EXPIRES)]


def test_build_inline_accepts_single_statement_object():
    source = {"Version": "2012-10-17", "Statement": READ_S3}
    assert build_inline(source, EXPIRES)["Statement"] == [READ_S3, expiry_statement(EXPIRES)]


def test_build_inline_replaces_old_expiry():
    old = expiry_statement(EXPIRES - timedelta(hours=3))
    source = {"Version": "2012-10-17", "Statement": [old, READ_S3]}
    assert build_inline(source, EXPIRES)["Statement"] == [READ_S3, expiry_statement(EXPIRES)]


def test_build_inline_does_not_modify_source():
    source = {"Version": "2012-10-17", "Statement": [READ_S3]}
    build_inline(source, EXPIRES)
    assert source == {"Version": "2012-10-17", "Statement": [READ_S3]}


def _inline_with_chars(target: int) -> dict:
    """An inline whose serialized form, expiry included, has exactly `target`
    non-whitespace characters."""
    base = build_inline({"Version": "2012-10-17", "Statement": [{"Sid": ""} | READ_S3]}, EXPIRES)
    missing = target - len("".join(serialize(base).split()))
    base["Statement"][0]["Sid"] = "x" * missing
    return base


def test_inline_at_char_limit_passes():
    assert check_limits(_inline_with_chars(10240), managed_count=0) == []


def test_inline_over_char_limit_reports_excess():
    errors = check_limits(_inline_with_chars(10241), managed_count=0)
    assert len(errors) == 1
    assert "10241" in errors[0] and "10240" in errors[0]


def test_whitespace_does_not_count_as_characters_but_counts_as_bytes():
    padding = " " * 33000
    inline = build_inline(
        {"Version": "2012-10-17", "Statement": [READ_S3 | {"Sid": "a" + padding + "b"}]}, EXPIRES
    )
    errors = check_limits(inline, managed_count=0)
    assert len(errors) == 1
    assert "bytes" in errors[0]


def test_managed_policy_limit():
    inline = build_inline(None, EXPIRES)
    assert check_limits(inline, managed_count=10) == []
    errors = check_limits(inline, managed_count=11)
    assert len(errors) == 1 and "11" in errors[0]


def test_serialize_is_ascii_and_compact():
    text = serialize({"Sid": "caffè"})
    assert text == '{"Sid":"caff\\u00e8"}'
    assert json.loads(text) == {"Sid": "caffè"}


@pytest.mark.parametrize(
    ("seconds", "expected"),
    [(300, "PT1H"), (3600, "PT1H"), (3601, "PT2H"), (14400, "PT4H"), (43200, "PT12H")],
)
def test_session_duration_rounds_up_to_the_hour(seconds, expected):
    assert session_duration(seconds) == expected


@pytest.mark.parametrize("seconds", [0, -1, 43201])
def test_session_duration_rejects_out_of_range(seconds):
    with pytest.raises(ValueError):
        session_duration(seconds)


def test_sanitize_description_drops_disallowed_characters():
    assert sanitize_description("fix prod 🔥 incident — now") == "fix prod incident now"


def test_sanitize_description_keeps_latin1_letters():
    assert sanitize_description("perché è rotto") == "perché è rotto"


def test_sanitize_description_collapses_newlines_and_spaces():
    assert sanitize_description("line one\n\nline\ttwo  ") == "line one line two"


def test_sanitize_description_truncates_to_700():
    assert len(sanitize_description("a" * 5000)) == 700


def test_sanitize_description_of_only_emoji_is_empty():
    assert sanitize_description("🔥🔥") == ""


def test_summarize_without_inline_is_empty():
    assert summarize(None) == []


def test_summarize_normalizes_actions_and_resources_to_lists():
    inline = {
        "Version": "2012-10-17",
        "Statement": [
            READ_S3,
            {
                "Effect": "Deny",
                "Action": ["ec2:TerminateInstances", "ec2:StopInstances"],
                "Resource": "*",
                "Condition": {"StringEquals": {"aws:RequestedRegion": "eu-west-1"}},
            },
        ],
    }
    assert summarize(inline) == [
        {
            "effect": "Allow",
            "actions": ["s3:GetObject"],
            "resources": ["arn:aws:s3:::bucket/*"],
            "conditions": None,
        },
        {
            "effect": "Deny",
            "actions": ["ec2:TerminateInstances", "ec2:StopInstances"],
            "resources": ["*"],
            "conditions": {"StringEquals": {"aws:RequestedRegion": "eu-west-1"}},
        },
    ]


def test_summarize_marks_not_action_statements():
    inline = {
        "Version": "2012-10-17",
        "Statement": {"Effect": "Allow", "NotAction": "iam:*", "NotResource": "arn:x"},
    }
    assert summarize(inline) == [
        {
            "effect": "Allow",
            "notActions": ["iam:*"],
            "notResources": ["arn:x"],
            "actions": [],
            "resources": [],
            "conditions": None,
        }
    ]


def _grant(**changes):
    from aws_elevate.core.models import (
        Approval,
        Grant,
        Group,
        PermissionSetRef,
        Principal,
        State,
    )

    base = Grant(
        id="abcde23456",
        state=State.ACTIVATING,
        rule_id="backend-ssm-prod",
        approval=Approval.MANUAL,
        config_version=3,
        requester=Principal(user_id="u-1", user_name="user@example.com"),
        approver_group=Group(id="g-2", name="leads"),
        source_ps=PermissionSetRef(
            arn="arn:aws:sso:::permissionSet/ssoins-1/ps-1", name="ssm-write"
        ),
        account="111111111111",
        duration_seconds=3600,
        reason="incident",
        privileged=True,
        created_at="2026-11-15T09:59:00Z",
        approver=Principal(user_id="u-2", user_name="lead@example.com"),
    )
    import dataclasses

    return dataclasses.replace(base, **changes)


def test_ephemeral_tags_follow_design():
    from aws_elevate.core.policy import ephemeral_tags

    assert ephemeral_tags(_grant()) == {
        "jit": "true",
        "jit:grantId": "abcde23456",
        "jit:requester": "user@example.com",
        "jit:approver": "lead@example.com",
        "jit:rule": "backend-ssm-prod",
        "jit:source": "ssm-write",
        "jit:account": "111111111111",
        "jit:configVersion": "3",
        "jit:privileged": "true",
    }


def test_ephemeral_tags_auto_approval():
    from aws_elevate.core.policy import ephemeral_tags

    tags = ephemeral_tags(_grant(approver=None, privileged=False))
    assert tags["jit:approver"] == "auto"
    assert tags["jit:privileged"] == "false"


def test_tag_values_drop_characters_identity_center_rejects():
    from aws_elevate.core.models import Principal
    from aws_elevate.core.policy import ephemeral_tags

    tags = ephemeral_tags(_grant(requester=Principal(user_id="u-1", user_name="o'brien#1 x")))
    assert tags["jit:requester"] == "obrien1 x"
