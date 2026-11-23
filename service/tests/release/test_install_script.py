"""install.sh: what it prints when the executable is installed."""

import hashlib
import os
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "install.sh"


def test_install_script_ends_with_the_two_next_steps(tmp_path: Path) -> None:
    kernel = subprocess.check_output(["uname", "-s"], text=True).strip()
    system = {"Linux": "linux", "Darwin": "darwin"}[kernel]
    machine = subprocess.check_output(["uname", "-m"], text=True).strip()
    arch = "arm64" if machine in ("aarch64", "arm64") else "amd64"
    asset = f"aws-elevate_{system}_{arch}"
    release = tmp_path / "releases" / "download" / "v1.2.3"
    release.mkdir(parents=True)
    binary = b"#!/bin/sh\n"
    (release / asset).write_bytes(binary)
    (release / "checksums.txt").write_text(f"{hashlib.sha256(binary).hexdigest()}  {asset}\n")

    result = subprocess.run(
        ["sh", str(SCRIPT)],
        env={
            **os.environ,
            "VERSION": "1.2.3",
            "PREFIX": str(tmp_path / "bin"),
            "AWS_ELEVATE_RELEASES": f"file://{tmp_path / 'releases'}",
        },
        capture_output=True,
        text=True,
    )

    assert result.returncode == 0, result.stderr
    assert result.stdout.splitlines()[-2:] == [
        "Next, first install in your organization: aws-elevate install --profile <admin profile>",
        "Next, Elevate already installed: aws-elevate setup",
    ]
