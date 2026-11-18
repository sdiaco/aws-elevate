"""The real client executable against the e2e stack: the commands a user and an admin
type. `make e2e` builds it into build/aws-elevate first.

The executable runs with a temporary HOME holding a copy of ~/.aws/config and a link to
the SSO token cache: the alias, the client settings and the jit-* profiles it writes
never touch the maintainer's files."""

import json
import os
import re
import shutil
import subprocess
import time
from pathlib import Path

import harness
import pytest
from harness import MANUAL_RULE_ID, RULE_ID

CLI = harness.CLI
REASON = "aws-elevate e2e"


@pytest.fixture(scope="module")
def home(tmp_path_factory):
    if not CLI.is_file():
        pytest.fail(f"{CLI} is missing: run the e2e tests through `make e2e`")
    home = tmp_path_factory.mktemp("home")
    real = Path.home() / ".aws"
    (home / ".aws").mkdir()
    shutil.copy(os.environ.get("AWS_CONFIG_FILE", real / "config"), home / ".aws" / "config")
    (home / ".aws" / "sso").symlink_to(real / "sso")
    return home


def cli(home: Path, *args: str, profile: str | None = None) -> subprocess.CompletedProcess:
    env = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
    env.update(HOME=str(home), AWS_CONFIG_FILE=str(home / ".aws" / "config"))
    env.pop("XDG_CONFIG_HOME", None)
    cmd = [str(CLI), *args, "--profile", profile or harness.env("PROFILE")]
    return subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=600)


def ok(result: subprocess.CompletedProcess) -> subprocess.CompletedProcess:
    assert result.returncode == 0, f"exit {result.returncode}\n{result.stdout}\n{result.stderr}"
    return result


def grant_id(text: str) -> str:
    return re.search(r"(?:Request|Grant) (\S+) ", text).group(1)


@pytest.fixture(scope="module")
def set_up(org, home):
    out = ok(cli(home, "setup")).stdout
    assert harness.env("SERVICE_ACCOUNT") in out
    assert "elevate" in (home / ".aws" / "cli" / "alias").read_text()
    return home


def test_setup_is_idempotent(set_up):
    alias = (set_up / ".aws" / "cli" / "alias").read_text()
    ok(cli(set_up, "setup"))
    assert (set_up / ".aws" / "cli" / "alias").read_text() == alias


def test_auto_request_profile_status_and_revoke(org, set_up):
    assert RULE_ID in ok(cli(set_up, "request")).stdout

    done = ok(cli(set_up, "request", RULE_ID, "--account", harness.env("TARGET_ACCOUNT"),
                  "--duration", "10m", "--reason", REASON))  # fmt: skip
    gid = grant_id(done.stderr)
    profile = re.search(r"^--profile (\S+)$", done.stdout, re.M).group(1)

    # The written profile works at once: the client waited until AWS accepted the role.
    who = subprocess.run(
        ["aws", "sts", "get-caller-identity", "--profile", profile, "--output", "json"],
        env={**{k: v for k, v in os.environ.items() if not k.startswith("AWS_")},
             "HOME": str(set_up), "AWS_CONFIG_FILE": str(set_up / ".aws" / "config")},
        capture_output=True, text=True, timeout=60,
    )  # fmt: skip
    assert who.returncode == 0, who.stderr
    assert json.loads(who.stdout)["Account"] == harness.env("TARGET_ACCOUNT")

    status = ok(cli(set_up, "status")).stdout
    assert gid in status and "active" in status

    ok(cli(set_up, "revoke", gid, "--reason", REASON, "--yes"))
    assert org.wait_state(gid, "closed", timeout_s=180)["closeReason"] == "revoked"


def test_manual_request_returns_and_cannot_be_self_approved(org, set_up):
    waiting = ok(cli(set_up, "request", MANUAL_RULE_ID, "--account", harness.env("TARGET_ACCOUNT"),
                     "--reason", REASON))  # fmt: skip
    assert "waiting for approval" in waiting.stderr
    gid = grant_id(waiting.stderr)
    try:
        refused = cli(set_up, "approve", gid, "--yes")
        assert refused.returncode != 0, refused.stdout
    finally:
        ok(cli(set_up, "revoke", gid, "--reason", REASON, "--yes"))
    assert org.wait_state(gid, "closed", timeout_s=120)["closeReason"] == "cancelled"


@pytest.mark.skipif(
    not harness.approval_enabled(),
    reason="approval by a second user is opt-in: E2E_APPROVAL=1 with APPROVER_PROFILE",
)
def test_manual_request_approved_by_another_user(org, set_up):
    approver = harness.env("APPROVER_PROFILE")
    ok(cli(set_up, "setup", profile=approver))
    waiting = ok(cli(set_up, "request", MANUAL_RULE_ID, "--account", harness.env("TARGET_ACCOUNT"),
                     "--reason", REASON))  # fmt: skip
    gid = grant_id(waiting.stderr)
    ok(cli(set_up, "approve", gid, "--yes", profile=approver))
    org.wait_state(gid, "active", timeout_s=180)
    ok(cli(set_up, "revoke", gid, "--reason", REASON, "--yes"))
    assert org.wait_state(gid, "closed", timeout_s=180)["closeReason"] == "revoked"


def test_admin_rules_save_export_and_refusal(org, set_up, tmp_path):
    exported = tmp_path / "rules.json"
    ok(cli(set_up, "admin", "export", str(exported)))
    doc = json.loads(exported.read_text())
    assert [r["id"] for r in doc["rules"]] == [r["id"] for r in org.rules_doc["rules"]]

    # export indents the document, the seeded version is compact: other bytes, so a new
    # version, which the service then uses.
    published = ok(cli(set_up, "admin", "rules", "save", str(exported))).stdout
    version = int(re.search(r"Published rules version (\d+)", published).group(1))
    org.config_version = version

    # The same file again: AppConfig creates no version.
    same = ok(cli(set_up, "admin", "rules", "save", str(exported))).stdout
    assert f"version {version} already has this content" in same, same

    broken = tmp_path / "broken.json"
    broken.write_text(json.dumps({"schemaVersion": 1, "rules": [{"id": "e2e-broken"}]}))
    refused = cli(set_up, "admin", "rules", "save", str(broken))
    assert refused.returncode == 1, refused.stdout
    assert "was not published" in refused.stderr
    # The api reads new versions within about 30 seconds (its cache).
    deadline = time.monotonic() + 60
    while f"Rules version {version}:" not in ok(cli(set_up, "admin", "rules", "list")).stdout:
        assert time.monotonic() < deadline, f"the service never used version {version}"
        time.sleep(5)


def test_admin_doctor(set_up):
    result = cli(set_up, "admin", "doctor")
    # One step per check: "  ✓ name   detail", with ✗ for a failure and ! for a warning.
    rows = {}
    for line in result.stdout.splitlines():
        if step := re.match(r"\s*([✓✗!]) (\S.*?)(?:\s{3,}|$)", line):
            rows.setdefault(step.group(2), step.group(1))
    for name in ("versions", "stack"):
        assert rows.get(name) == "✓", result.stdout
    assert "✗" not in {s for n, s in rows.items() if n != "alerts"}, result.stdout
    # The e2e stack subscribes user@example.com to alerts, never confirmed: the only
    # failure doctor may report here.
    assert result.returncode == (1 if rows.get("alerts") == "✗" else 0), result.stdout
