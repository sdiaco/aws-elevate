"""Inline policy of the ephemeral permission set (design §5.1, §5.4)."""

import json
import math
import re
from datetime import UTC, datetime

EXPIRY_SID = "JitExpiry"
POLICY_VERSION = "2012-10-17"

# Permission set limits documented by AWS, not adjustable (design §5.4).
MAX_INLINE_CHARS = 10_240  # excluding whitespace
MAX_INLINE_BYTES = 32_768
# IAM quota on managed policies per role (adjustable by the organization, not by us).
MAX_MANAGED_POLICIES = 10

MAX_SESSION_SECONDS = 12 * 3600

# Identity Center accepts only these characters in a permission set description
# (botocore model, shape PermissionSetDescription), up to 700 of them.
_DESCRIPTION_DISALLOWED = re.compile(r"[^\u0009\u000A\u000D -~¡-ÿ]")
_MAX_DESCRIPTION = 700


def expiry_statement(expires_at: datetime) -> dict:
    if expires_at.tzinfo is None:
        raise ValueError("expires_at must be timezone-aware")
    stamp = expires_at.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")
    return {
        "Sid": EXPIRY_SID,
        "Effect": "Deny",
        "Action": "*",
        "Resource": "*",
        "Condition": {"DateGreaterThan": {"aws:CurrentTime": stamp}},
    }


def _statements(inline: dict | None) -> list[dict]:
    if inline is None:
        return []
    statements = inline.get("Statement", [])
    return [statements] if isinstance(statements, dict) else list(statements)


def build_inline(source_inline: dict | None, expires_at: datetime) -> dict:
    """Source statements plus the expiry statement. An expiry left over from an
    earlier copy is replaced, never duplicated."""
    kept = [s for s in _statements(source_inline) if s.get("Sid") != EXPIRY_SID]
    version = (source_inline or {}).get("Version", POLICY_VERSION)
    return {"Version": version, "Statement": [*kept, expiry_statement(expires_at)]}


def serialize(inline: dict) -> str:
    """The exact text sent to Identity Center. ASCII only: the API rejects
    characters above U+00FF in a policy document."""
    return json.dumps(inline, separators=(",", ":"))


def check_limits(inline: dict, managed_count: int) -> list[str]:
    """Human-readable violations; empty when the permission set can be created."""
    text = serialize(inline)
    chars = len("".join(text.split()))
    size = len(text.encode("utf-8"))
    errors = []
    if chars > MAX_INLINE_CHARS:
        errors.append(
            f"inline policy has {chars} characters excluding whitespace, "
            f"the limit is {MAX_INLINE_CHARS} ({chars - MAX_INLINE_CHARS} too many)"
        )
    if size > MAX_INLINE_BYTES:
        errors.append(
            f"inline policy is {size} bytes, the limit is {MAX_INLINE_BYTES} "
            f"({size - MAX_INLINE_BYTES} too many)"
        )
    if managed_count > MAX_MANAGED_POLICIES:
        errors.append(
            f"{managed_count} managed policies, the limit is {MAX_MANAGED_POLICIES} per role"
        )
    return errors


def session_duration(seconds: int) -> str:
    """Session length of the ephemeral permission set: the grant duration rounded up
    to the hour, between 1h and 12h. The expiry statement still ends access on time."""
    if not 0 < seconds <= MAX_SESSION_SECONDS:
        raise ValueError(f"duration must be between 1 and {MAX_SESSION_SECONDS} seconds")
    return f"PT{max(1, math.ceil(seconds / 3600))}H"


def sanitize_description(reason: str) -> str:
    """Request reason made acceptable as a description: disallowed characters dropped,
    whitespace collapsed, truncated. May return "" (the caller then omits it)."""
    cleaned = " ".join(_DESCRIPTION_DISALLOWED.sub("", reason).split())
    return cleaned[:_MAX_DESCRIPTION].rstrip()


def _as_list(value) -> list:
    if value is None:
        return []
    return [value] if isinstance(value, str) else list(value)


def summarize(inline: dict | None) -> list[dict]:
    """One entry per statement, for "what you will get" (design §11.2)."""
    summary = []
    for stmt in _statements(inline):
        entry = {
            "effect": stmt.get("Effect"),
            "actions": _as_list(stmt.get("Action")),
            "resources": _as_list(stmt.get("Resource")),
            "conditions": stmt.get("Condition"),
        }
        # NotAction / NotResource invert the meaning; hiding them would make an
        # allow-everything-except statement look empty.
        if "NotAction" in stmt:
            entry["notActions"] = _as_list(stmt["NotAction"])
        if "NotResource" in stmt:
            entry["notResources"] = _as_list(stmt["NotResource"])
        summary.append(entry)
    return summary
