"""dev/release-plan.sh with a fake gh: what the release workflow publishes after a push
to main."""

import os
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
VERSION = (
    (ROOT / "service/src/aws_elevate/version.py")
    .read_text()
    .split('INFRA_VERSION = "')[1]
    .split('"')[0]
)

FAKE_GH = """#!/usr/bin/env bash
case "$1 $2" in
"release view") [ "$RELEASED" = yes ] && exit 0 || exit 1 ;;
esac
case "$2" in
*/pulls) printf '%s' "$PR" ;;
*/check-runs*) printf '%s' "$CHECK" ;;
esac
"""


def plan(tmp_path, released="no", pr="7 abc123 release:fix", check="success"):
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    (bin_dir / "gh").write_text(FAKE_GH)
    (bin_dir / "gh").chmod(0o755)
    output = tmp_path / "output"
    output.touch()
    env = {
        **os.environ,
        "PATH": f"{bin_dir}:{os.environ['PATH']}",
        "GITHUB_REPOSITORY": "owner/repo",
        "GITHUB_SHA": "def456",
        "GITHUB_OUTPUT": str(output),
        "RELEASED": released,
        "PR": pr,
        "CHECK": check,
    }
    result = subprocess.run(
        [str(ROOT / "dev/release-plan.sh")], capture_output=True, text=True, env=env, check=False
    )
    return result, dict(line.split("=", 1) for line in output.read_text().splitlines())


@pytest.mark.parametrize("label", ["security", "fix", "feature"])
def test_a_labelled_and_checked_pull_request_gives_version_tested_commit_and_type(tmp_path, label):
    result, out = plan(tmp_path, pr=f"7 abc123 release:{label}")
    assert result.returncode == 0, result.stderr
    assert out == {"version": VERSION, "tested": "abc123", "type": label}


def test_an_already_published_version_releases_nothing(tmp_path):
    result, out = plan(tmp_path, released="yes", pr="", check="")
    assert result.returncode == 0 and "already published" in result.stdout
    assert out == {}


def test_a_commit_without_a_merged_pull_request_fails(tmp_path):
    result, out = plan(tmp_path, pr="")
    assert result.returncode != 0 and "merged pull request" in result.stderr
    assert out == {}


@pytest.mark.parametrize(
    ("labels", "message"),
    [("", "no release label"), ("release:fix,release:feature", "more than one"),
     ("release:major", "unknown")],
)  # fmt: skip
def test_the_release_label_must_be_exactly_one_known_type(tmp_path, labels, message):
    result, out = plan(tmp_path, pr=f"7 abc123 {labels}".rstrip() + " ")
    assert result.returncode != 0 and message in result.stderr
    assert out == {}


@pytest.mark.parametrize("check", ["", "failure", "cancelled"])
def test_a_tested_commit_without_a_passing_check_fails(tmp_path, check):
    result, out = plan(tmp_path, check=check)
    assert result.returncode != 0 and "make check did not pass" in result.stderr
    assert out == {}
