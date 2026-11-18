"""Rules in force for api: latest valid version, 30 s cache, invalid
versions recorded and reported once, author from CloudTrail when it is available."""

import copy

import pytest
from botocore.exceptions import ClientError

from aws_elevate.adapters.appconfig import NotJson
from aws_elevate.adapters.events import EventPublishFailed
from aws_elevate.api.config_cache import ConfigCache, ConfigUnavailable

AUTHOR = "arn:aws:sts::111111111111:assumed-role/AWSReservedSSO_ElevateAdmin_x/user@example.com"
RULE = {
    "id": "backend-ssm-prod",
    "group": {"id": "g-1", "name": "backend-devs"},
    "permissionSet": {"arn": "arn:aws:sso:::permissionSet/ssoins-1/ps-1", "name": "ssm-write"},
    "accounts": [{"id": "111111111111", "name": "prod"}],
    "duration": {"defaultSeconds": 3600, "maxSeconds": 14400},
    "approval": "auto",
    "approverGroup": None,
    "privileged": False,
}
VALID = {"schemaVersion": 1, "rules": [RULE]}
BROKEN = {"schemaVersion": 1, "rules": [{**RULE, "approval": "nope"}]}


class FakeAppConfig:
    def __init__(self):
        self.docs: dict[int, object] = {}
        self.schema_invalid: set[int] = set()
        self.validated: list[int] = []
        self.list_calls = 0

    def list_versions(self):
        self.list_calls += 1
        return sorted(self.docs, reverse=True)

    def get(self, n):
        if self.docs[n] == "not json":
            raise NotJson(f"config version {n} is not JSON")
        return copy.deepcopy(self.docs[n])

    def validate(self, n):
        self.validated.append(n)
        return (
            (False, "JSON Schema validation failed") if n in self.schema_invalid else (True, None)
        )


class FakeTable:
    """Shared between containers, like the real table."""

    def __init__(self):
        self.configs = {}

    def get_config(self, n):
        return self.configs.get(n)

    def put_config(self, cv):
        if cv.number in self.configs:
            return False
        self.configs[cv.number] = cv
        return True

    def set_config_author(self, n, author, created_at):
        import dataclasses

        self.configs[n] = dataclasses.replace(
            self.configs[n], author=author, created_at=created_at, verified=True
        )

    def claim_rejected_event(self, n):
        import dataclasses

        if self.configs[n].rejected_event_sent:
            return False
        self.configs[n] = dataclasses.replace(self.configs[n], rejected_event_sent=True)
        return True

    def release_rejected_event(self, n):
        import dataclasses

        self.configs[n] = dataclasses.replace(self.configs[n], rejected_event_sent=False)


class FakeTrail:
    def __init__(self):
        self.authors = {}
        self.calls = 0

    def version_author(self, n):
        self.calls += 1
        return self.authors.get(n)


class FakeEvents:
    def __init__(self):
        self.sent = []
        self.fail = None

    def put_config_rejected(self, n, author, errors):
        if self.fail:
            raise self.fail
        self.sent.append((n, author, list(errors)))


class Clock:
    def __init__(self):
        self.t = 1000.0

    def __call__(self):
        return self.t


@pytest.fixture
def aws():
    ac, table, trail, events = FakeAppConfig(), FakeTable(), FakeTrail(), FakeEvents()
    clock = Clock()

    def container(listed=50):
        return ConfigCache(ac, table, trail, events, versions_listed=listed, clock=clock)

    return ac, table, trail, events, clock, container


def test_latest_valid_version(aws):
    ac, table, trail, _, _, container = aws
    ac.docs = {1: VALID}
    trail.authors[1] = (AUTHOR, "2026-11-15T10:00:00Z")
    version, rules = container().current()
    assert version == 1
    assert [r.id for r in rules] == ["backend-ssm-prod"]
    assert table.configs[1].valid and table.configs[1].verified
    assert table.configs[1].author == AUTHOR


def test_cached_for_30_seconds(aws):
    ac, _, _, _, clock, container = aws
    ac.docs = {1: VALID}
    cache = container()
    cache.current()
    ac.docs[2] = {"schemaVersion": 1, "rules": []}
    clock.t += 29
    assert cache.current()[0] == 1
    assert ac.list_calls == 1
    clock.t += 2
    assert cache.current()[0] == 2


def test_unchanged_version_is_not_validated_again(aws):
    ac, _, _, _, clock, container = aws
    ac.docs = {1: VALID}
    cache = container()
    cache.current()
    clock.t += 31
    cache.current()
    assert ac.validated == [1]


@pytest.mark.parametrize(
    ("setup", "fragment"),
    [
        (lambda ac: ac.docs.__setitem__(2, BROKEN), "approval"),
        (lambda ac: ac.docs.__setitem__(2, "not json"), "not JSON"),
        (lambda ac: (ac.docs.__setitem__(2, VALID), ac.schema_invalid.add(2)), "JSON Schema"),
    ],
)
def test_invalid_version_keeps_the_last_valid_one(aws, setup, fragment):
    ac, table, _, events, _, container = aws
    ac.docs = {1: VALID}
    setup(ac)
    assert container().current()[0] == 1
    assert table.configs[2].valid is False
    assert any(fragment in e for e in table.configs[2].errors)
    assert len(events.sent) == 1
    assert events.sent[0][0] == 2


def test_one_event_after_two_cold_starts(aws):
    ac, _, _, events, _, container = aws
    ac.docs = {1: VALID, 2: BROKEN}
    container().current()
    container().current()
    assert [e[0] for e in events.sent] == [2]


@pytest.mark.parametrize(
    "failure",
    [
        EventPublishFailed("bus unavailable"),
        ClientError({"Error": {"Code": "InternalException", "Message": "x"}}, "PutEvents"),
    ],
)
def test_event_that_fails_is_retried_on_the_next_read(aws, failure):
    ac, table, _, events, clock, container = aws
    ac.docs = {1: VALID, 2: BROKEN}
    events.fail = failure
    cache = container()
    assert cache.current()[0] == 1
    assert table.configs[2].rejected_event_sent is False
    events.fail = None
    clock.t += 31
    cache.current()
    assert [e[0] for e in events.sent] == [2]


def test_no_valid_version(aws):
    ac, _, _, _, _, container = aws
    ac.docs = {1: BROKEN}
    with pytest.raises(ConfigUnavailable):
        container().current()


def test_no_version_at_all(aws):
    with pytest.raises(ConfigUnavailable):
        aws[5]().current()


def test_author_not_available_yet_is_retried_later(aws):
    ac, table, trail, _, clock, container = aws
    ac.docs = {1: VALID}
    cache = container()
    cache.current()
    assert table.configs[1].verified is False
    trail.authors[1] = (AUTHOR, "2026-11-15T10:00:00Z")
    clock.t += 31
    cache.current()
    assert table.configs[1].verified is True
    calls = trail.calls
    clock.t += 31
    cache.current()
    assert trail.calls == calls  # verified: CloudTrail is not asked again


def test_version_recorded_by_another_container_is_not_reevaluated(aws):
    ac, table, _, _, _, container = aws
    ac.docs = {1: VALID, 2: BROKEN}
    container().current()
    ac.validated.clear()
    container().current()
    assert ac.validated == []


def test_new_valid_version_replaces_the_cached_one(aws):
    ac, _, _, _, clock, container = aws
    ac.docs = {1: VALID}
    cache = container()
    cache.current()
    ac.docs[2] = {"schemaVersion": 1, "rules": [{**RULE, "id": "other"}]}
    clock.t += 31
    version, rules = cache.current()
    assert (version, [r.id for r in rules]) == (2, ["other"])


# ---------- version list and content (GET /config/versions) ----------


def test_versions_lists_versions_newest_first_with_their_record(aws):
    ac, table, trail, _, _, container = aws
    ac.docs = {1: VALID, 2: BROKEN}
    trail.authors[1] = (AUTHOR, "2026-11-15T10:00:00Z")
    cache = container()
    cache.current()
    listed = cache.versions()
    assert [v.number for v in listed] == [2, 1]
    assert listed[0].valid is False
    assert listed[1].author == AUTHOR


def test_versions_lists_only_the_newest_ones_and_reads_no_older_record(aws):
    ac, table, _, _, _, container = aws
    ac.docs = {n: VALID for n in range(1, 61)}
    cache = container(listed=20)
    cache.current()
    listed = cache.versions()
    assert [v.number for v in listed] == list(range(60, 40, -1))
    # Older versions are never read or recorded: the list stays as fast with thousands.
    assert all(n not in table.configs for n in range(1, 41))


def test_versions_evaluates_a_version_never_seen_without_asking_cloudtrail(aws):
    ac, table, trail, _, _, container = aws
    ac.docs = {1: BROKEN, 2: VALID}
    cache = container()
    cache.current()  # stops at 2, never looks at 1
    calls = trail.calls
    listed = cache.versions()
    assert [(v.number, v.valid) for v in listed] == [(2, True), (1, False)]
    assert table.configs[1].valid is False
    # Listing never queries CloudTrail: LookupEvents allows 2 calls per second.
    assert trail.calls == calls


def test_version_returns_record_and_content(aws):
    ac, _, _, _, _, container = aws
    ac.docs = {1: VALID}
    record, doc = container().version(1)
    assert record.number == 1
    assert record.valid is True
    assert doc == VALID


def test_version_that_is_not_json_has_no_content(aws):
    ac, _, _, _, _, container = aws
    ac.docs = {1: "not json"}
    record, doc = container().version(1)
    assert record.valid is False
    assert doc is None


def test_unknown_version_is_none(aws):
    ac, _, _, _, _, container = aws
    ac.docs = {1: VALID}
    assert container().version(7) is None
