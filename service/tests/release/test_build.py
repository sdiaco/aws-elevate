"""dev/build.sh: the files of one release (design §12.1). It needs Go, present wherever
make check runs (host or development container)."""

import hashlib
import platform
import re
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "dev" / "build.sh"
VERSION = re.search(
    r'^INFRA_VERSION = "(.*)"$', (ROOT / "service/src/aws_elevate/version.py").read_text(), re.M
).group(1)
FILES = {
    "lambda.zip",
    "elevate.yaml",
    "elevate-guardrails.yaml",
    "aws-elevate_linux_amd64",
    "aws-elevate_linux_arm64",
    "aws-elevate_darwin_amd64",
    "aws-elevate_darwin_arm64",
    "checksums.txt",
}


def build(version: str, out: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        [str(SCRIPT), version, str(out)], capture_output=True, text=True, check=False
    )


@pytest.fixture(scope="module")
def release(tmp_path_factory) -> Path:
    out = tmp_path_factory.mktemp("first")
    result = build(VERSION, out)
    assert result.returncode == 0, result.stderr
    return out / f"v{VERSION}"


def test_build_writes_every_release_file(release):
    assert {p.name for p in release.iterdir()} == FILES


def test_checksums_cover_every_other_file(release):
    lines = (release / "checksums.txt").read_text().splitlines()
    sums = dict(reversed(line.split()) for line in lines)
    assert set(sums) == FILES - {"checksums.txt"}
    for name, digest in sums.items():
        assert hashlib.sha256((release / name).read_bytes()).hexdigest() == digest


def test_the_same_source_gives_the_same_files(release, tmp_path):
    result = build(VERSION, tmp_path)
    assert result.returncode == 0, result.stderr
    again = tmp_path / f"v{VERSION}"
    assert (again / "checksums.txt").read_text() == (release / "checksums.txt").read_text()


def test_templates_are_the_repository_ones(release):
    for name in ("elevate.yaml", "elevate-guardrails.yaml"):
        assert (release / name).read_bytes() == (ROOT / "infra" / name).read_bytes()


def test_the_executable_reports_the_release_version(release):
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
    system = platform.system().lower()
    out = subprocess.run(
        [str(release / f"aws-elevate_{system}_{arch}"), "--version"],
        capture_output=True,
        text=True,
        check=True,
    )
    assert VERSION in out.stdout


def test_a_version_the_code_does_not_declare_stops_the_build(tmp_path):
    result = build("9.9.9", tmp_path)
    assert result.returncode != 0
    assert "INFRA_VERSION" in result.stderr and "not 9.9.9" in result.stderr
    assert not (tmp_path / "v9.9.9").exists()


@pytest.mark.parametrize("version", ["1.2", "v1.2.3", "01.2.3", "1.2.3-rc1"])
def test_a_malformed_version_stops_the_build(tmp_path, version):
    result = build(version, tmp_path)
    assert result.returncode != 0
    assert "MAJOR.MINOR.PATCH" in result.stderr
