"""dev/release.sh, run in a throwaway git repository with fake aws and gh commands that
record their arguments: nothing reaches AWS or GitHub."""

import hashlib
import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
VERSION = "1.2.3"
BUCKETS = {"eu-west-1": "rel-eu", "us-east-1": "rel-us"}
UPLOADED = ("elevate.yaml", "elevate-guardrails.yaml", "lambda.zip")

FAKE = """#!/usr/bin/env bash
{ printf '%s' "$(basename "$0")"; printf ' %q' "$@"; echo; } >>"$CALLS"
if [ "$(basename "$0")" = gh ] && [ "$1" = api ]; then
	echo "$E2E_STATE"
	exit 0
fi
if [ "$(basename "$0")" = aws ] && [ "$2" = head-object ]; then
	case " $* " in *" $EXISTING "*) exit 0 ;; esac
	exit 254
fi
"""


def git(repo: Path, *args: str) -> str:
    return subprocess.run(
        ["git", "-C", str(repo), *args], capture_output=True, text=True, check=True
    ).stdout.strip()


@pytest.fixture
def repo(tmp_path) -> Path:
    repo = tmp_path / "repo"
    (repo / "dev").mkdir(parents=True)
    shutil.copy(ROOT / "dev" / "release.sh", repo / "dev" / "release.sh")
    (repo / "contract").mkdir()
    (repo / "contract" / "release-buckets.json").write_text(json.dumps(BUCKETS))
    (repo / "client/internal/version").mkdir(parents=True)
    (repo / "client/internal/version/version.go").write_text('var MinInfra = "1.0.0"\n')
    (repo / "service/src/aws_elevate").mkdir(parents=True)
    (repo / "service/src/aws_elevate/version.py").write_text('MIN_CLIENT = "1.1.0"\n')
    (repo / ".gitignore").write_text("build/\n")
    git(repo, "init", "-q", "-b", "main")
    git(repo, "add", ".")
    git(repo, "-c", "user.name=t", "-c", "user.email=user@example.com", "commit", "-qm", "init")

    built = repo / "build/release" / f"v{VERSION}"
    built.mkdir(parents=True)
    names = [*UPLOADED, "aws-elevate_linux_amd64"]
    for name in names:
        (built / name).write_text(name)
    (built / "checksums.txt").write_text(
        "".join(f"{hashlib.sha256(name.encode()).hexdigest()}  {name}\n" for name in names)
    )

    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for tool in ("aws", "gh"):
        (bin_dir / tool).write_text(FAKE)
        (bin_dir / tool).chmod(0o755)
    return repo


def run(
    repo: Path, *args: str, existing: str = "none", e2e: str = "success"
) -> tuple[subprocess.CompletedProcess, list]:
    calls = repo.parent / "calls"
    calls.touch()
    env = {
        **os.environ,
        "PATH": f"{repo.parent / 'bin'}:{os.environ['PATH']}",
        "CALLS": str(calls),
        "EXISTING": existing,
        "E2E_STATE": e2e,
        "GITHUB_REPOSITORY": "owner/repo",
    }
    result = subprocess.run(
        [str(repo / "dev/release.sh"), *args], capture_output=True, text=True, env=env, check=False
    )
    lines = calls.read_text().splitlines()
    status = f"gh api repos/owner/repo/commits/{head(repo)}/status"
    return result, [c for c in lines if not c.startswith(status)]


def head(repo: Path) -> str:
    return git(repo, "rev-parse", "HEAD")


def publish(repo: Path, step: str, last: str | None, **kw):
    args = [step, VERSION, head(repo)] + ([last] if last else [])
    return run(repo, *args, **kw)


def test_upload_puts_each_file_in_every_listed_bucket_without_overwriting(repo):
    result, calls = publish(repo, "upload", "pub")
    assert result.returncode == 0, result.stderr
    puts = [c for c in calls if " put-object " in c]
    assert len(puts) == len(BUCKETS) * len(UPLOADED)
    for region, bucket in BUCKETS.items():
        for name in UPLOADED:
            assert any(
                f"--region {region} --bucket {bucket} --key v{VERSION}/{name}" in c
                and "--if-none-match \\*" in c
                and "--profile pub" in c
                for c in puts
            )


def test_upload_writes_nothing_when_one_file_is_already_published(repo):
    result, calls = publish(repo, "upload", "pub", existing=f"v{VERSION}/lambda.zip")
    assert result.returncode != 0
    assert "never overwritten" in result.stderr
    assert not [c for c in calls if " put-object " in c]


def test_github_release_carries_the_header_lines_read_from_the_code(repo):
    result, calls = publish(repo, "github", "fix")
    assert result.returncode == 0, result.stderr
    (call,) = calls
    assert call.startswith(f"gh release create v{VERSION} ")
    assert "--notes $'type: fix\\nminInfra: 1.0.0\\nminClient: 1.1.0'" in call
    assert "aws-elevate_linux_amd64" in call and "checksums.txt" in call
    assert "lambda.zip" not in call


def test_github_refuses_an_unknown_type(repo):
    result, calls = publish(repo, "github", "major")
    assert result.returncode != 0 and "security, fix or feature" in result.stderr
    assert calls == []


def test_only_main_publishes(repo):
    git(repo, "checkout", "-q", "-b", "develop")
    result, calls = publish(repo, "upload", "pub")
    assert result.returncode != 0 and "from main only" in result.stderr
    assert calls == []


def test_a_dirty_tree_does_not_publish(repo):
    (repo / "contract/new.json").write_text("{}")
    result, calls = publish(repo, "upload", "pub")
    assert result.returncode != 0 and "has changes" in result.stderr
    assert calls == []


def test_a_build_that_does_not_match_its_checksums_does_not_publish(repo):
    (repo / "build/release" / f"v{VERSION}" / "lambda.zip").write_text("changed")
    result, calls = publish(repo, "upload", "pub")
    assert result.returncode != 0 and "does not match" in result.stderr
    assert calls == []


def test_upload_without_a_profile_uses_the_environment_credentials(repo):
    result, calls = publish(repo, "upload", None)
    assert result.returncode == 0, result.stderr
    assert calls and not [c for c in calls if "--profile" in c]


@pytest.mark.parametrize("state", ["", "failure", "pending"])
def test_without_a_successful_e2e_status_nothing_is_published(repo, state):
    result, calls = publish(repo, "github", "fix", e2e=state)
    assert result.returncode != 0 and "no successful e2e status" in result.stderr
    assert calls == []


def test_a_tested_commit_with_another_tree_does_not_publish(repo):
    tested = head(repo)
    (repo / "contract/new.json").write_text("{}")
    git(repo, "add", ".")
    git(repo, "-c", "user.name=t", "-c", "user.email=user@example.com", "commit", "-qm", "more")
    result, calls = run(repo, "upload", VERSION, tested, "pub")
    assert result.returncode != 0 and "did not test this code" in result.stderr
    assert calls == []


def test_a_detached_checkout_of_main_in_github_actions_publishes(repo, monkeypatch):
    git(repo, "checkout", "-q", "--detach")
    monkeypatch.setenv("GITHUB_REF", "refs/heads/main")
    result, _ = publish(repo, "upload", "pub")
    assert result.returncode == 0, result.stderr
