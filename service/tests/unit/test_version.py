import re

from aws_elevate import version

SEMVER = re.compile(r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")


def test_versions_are_semver():
    assert SEMVER.match(version.INFRA_VERSION)
    assert SEMVER.match(version.MIN_CLIENT)
