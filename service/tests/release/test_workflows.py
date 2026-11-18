"""Security rules of the GitHub Actions workflows: actions pinned by commit, read-only
token by default, no untrusted text in commands, no pull_request_target, and the same
tool versions as the development container."""

import re
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
WORKFLOWS = sorted((ROOT / ".github/workflows").glob("*.yml"))
DOCKERFILE = (ROOT / "dev/Dockerfile").read_text()


def load(path: Path) -> dict:
    # YAML 1.1 reads the key "on" as True.
    data = yaml.safe_load(path.read_text())
    data["on"] = data.pop(True)
    return data


def steps(workflow: dict) -> list[dict]:
    return [s for job in workflow["jobs"].values() for s in job["steps"]]


def test_there_are_workflows():
    assert {p.name for p in WORKFLOWS} == {"check.yml", "release.yml"}


@pytest.mark.parametrize("path", WORKFLOWS, ids=lambda p: p.name)
def test_every_action_is_pinned_to_a_full_commit(path):
    for line in path.read_text().splitlines():
        if re.match(r"\s*(- )?uses:", line):
            assert re.search(r"uses: [\w.-]+/[\w.-]+@[0-9a-f]{40} # v\d", line), line


@pytest.mark.parametrize("path", WORKFLOWS, ids=lambda p: p.name)
def test_the_token_is_read_only_unless_a_job_asks(path):
    assert load(path)["permissions"] == {"contents": "read"}


@pytest.mark.parametrize("path", WORKFLOWS, ids=lambda p: p.name)
def test_no_pull_request_target_and_no_persisted_credentials(path):
    workflow = load(path)
    assert "pull_request_target" not in workflow["on"]
    for step in steps(workflow):
        if step.get("uses", "").startswith("actions/checkout@"):
            assert step["with"]["persist-credentials"] is False


@pytest.mark.parametrize("path", WORKFLOWS, ids=lambda p: p.name)
def test_commands_never_interpolate_expressions(path):
    # Expressions go through env: interpolated into a script, a pull request title or a
    # branch name could run as a command.
    for step in steps(load(path)):
        assert "${{" not in step.get("run", ""), step["run"]


def test_only_the_release_job_gets_an_aws_token_or_writes_contents():
    for path in WORKFLOWS:
        for name, job in load(path)["jobs"].items():
            perms = job.get("permissions", {})
            if (path.name, name) == ("release.yml", "release"):
                assert perms["id-token"] == "write" and perms["contents"] == "write"
            else:
                assert "id-token" not in perms and perms.get("contents", "read") == "read"


def test_the_release_runs_only_on_main():
    assert load(ROOT / ".github/workflows/release.yml")["on"] == {"push": {"branches": ["main"]}}


def test_tool_versions_match_the_development_container():
    uv = re.search(r"astral-sh/uv:(\S+) ", DOCKERFILE).group(1)
    python = re.search(r"FROM python:(\d+\.\d+\.\d+)-", DOCKERFILE).group(1)
    go = re.search(r"ARG GO_VERSION=(\S+)", DOCKERFILE).group(1)
    go_mod = re.search(r"^go (\S+)$", (ROOT / "client/go.mod").read_text(), re.M).group(1)
    assert go == go_mod
    for path in WORKFLOWS:
        for step in steps(load(path)):
            uses = step.get("uses", "")
            if uses.startswith("astral-sh/setup-uv@"):
                assert step["with"]["version"] == uv
                assert step["with"]["python-version"] == python
            if uses.startswith("actions/setup-go@"):
                assert step["with"]["go-version-file"] == "client/go.mod"
