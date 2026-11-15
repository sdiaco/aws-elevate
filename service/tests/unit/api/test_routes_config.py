"""Rules version routes (design §7.3, §11.2): list with date and author, and content of
one version. Admin and auditor only, so the auditor sees the history without AWS access."""

import pytest

from aws_elevate.api.errors import ApiError
from aws_elevate.api.routes_config import ConfigRoutes

AUTHOR = "arn:aws:sts::111111111111:assumed-role/AWSReservedSSO_ElevateAdmin_x/user@example.com"
DOC = {"schemaVersion": 1, "rules": []}


@pytest.fixture
def config(fx):
    return fx.FakeConfig(
        version=2,
        records=[
            fx.config_version(3, valid=False, errors=("schema: bad",)),
            fx.config_version(2, author=AUTHOR, created_at="2026-11-15T10:00:00Z", verified=True),
            fx.config_version(1),
        ],
        docs={2: DOC, 3: None},
    )


def error_of(call) -> ApiError:
    with pytest.raises(ApiError) as caught:
        call()
    return caught.value


@pytest.mark.parametrize("who", ["ADMIN", "AUDITOR"])
def test_admin_and_auditor_list_the_versions(fx, config, who):
    body = ConfigRoutes(config).list(getattr(fx, who))
    assert body["current"] == 2
    assert body["versions"] == [
        {
            "number": 3,
            "valid": False,
            "errors": ["schema: bad"],
            "author": None,
            "createdAt": None,
            "verified": False,
        },
        {
            "number": 2,
            "valid": True,
            "errors": [],
            "author": AUTHOR,
            "createdAt": "2026-11-15T10:00:00Z",
            "verified": True,
        },
        {
            "number": 1,
            "valid": True,
            "errors": [],
            "author": None,
            "createdAt": None,
            "verified": False,
        },
    ]


def test_list_without_any_valid_version_has_no_current(fx):
    config = fx.FakeConfig(records=[fx.config_version(1, valid=False)], unavailable=True)
    assert ConfigRoutes(config).list(fx.ADMIN)["current"] is None


def test_auditor_reads_the_content_of_a_version(fx, config):
    body = ConfigRoutes(config).get(fx.AUDITOR, "2")
    assert body["version"]["number"] == 2
    assert body["version"]["author"] == AUTHOR
    assert body["doc"] == DOC


def test_content_that_is_not_json_is_null(fx, config):
    body = ConfigRoutes(config).get(fx.ADMIN, "3")
    assert body["doc"] is None
    assert body["version"]["valid"] is False


def test_unknown_version_is_not_found(fx, config):
    err = error_of(lambda: ConfigRoutes(config).get(fx.ADMIN, "9"))
    assert (err.status, err.code) == (404, "not_found")


@pytest.mark.parametrize("who", ["REQUESTER", "LEAD", "STRANGER"])
def test_others_cannot_read_versions(fx, config, who):
    identity = getattr(fx, who)
    for call in (
        lambda: ConfigRoutes(config).list(identity),
        lambda: ConfigRoutes(config).get(identity, "2"),
    ):
        err = error_of(call)
        assert (err.status, err.code) == (403, "forbidden")
