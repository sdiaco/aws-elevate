"""In-memory fakes behind the api routes, shared by the unit and the contract tests.
The table keeps the real lifecycle rules and offers only the reads the real adapter has,
so a route cannot pass with a Scan."""

import dataclasses
import sys
from datetime import UTC, datetime

import pytest

from aws_elevate.adapters.grants_table import Duplicate, StateConflict
from aws_elevate.adapters.sso_read import PsNotFound
from aws_elevate.api.config_cache import ConfigUnavailable
from aws_elevate.core.lifecycle import can_transition
from aws_elevate.core.models import (
    Account,
    Approval,
    ConfigVersion,
    Grant,
    Group,
    Identity,
    PermissionSetRef,
    Principal,
    Rule,
    SourcePs,
    State,
)
from aws_elevate.core.times import iso

NOW = datetime(2026, 11, 15, 10, 0, 0, tzinfo=UTC)
PS_ARN = "arn:aws:sso:::permissionSet/ssoins-1/ps-1"
TOKEN = "token-" + "t" * 30

REQUESTERS = Group(id="g-req", name="backend-devs")
LEADS = Group(id="g-lead", name="leads")

AUTO_RULE = Rule(
    id="ssm-dev",
    group=REQUESTERS,
    permission_set=PermissionSetRef(arn=PS_ARN, name="ssm-write"),
    accounts=(Account(id="111111111111", name="dev"),),
    default_seconds=3600,
    max_seconds=14400,
    approval=Approval.AUTO,
    approver_group=None,
    privileged=False,
)
MANUAL_RULE = dataclasses.replace(
    AUTO_RULE,
    id="ssm-prod",
    accounts=(Account(id="222222222222", name="prod"),),
    approval=Approval.MANUAL,
    approver_group=LEADS,
    privileged=True,
)

REQUESTER = Identity("u-1", "user@example.com", frozenset({"g-req"}), False, False)
LEAD = Identity("u-2", "lead@example.com", frozenset({"g-lead"}), False, False)
ADMIN = Identity("u-3", "admin@example.com", frozenset(), True, False)
AUDITOR = Identity("u-4", "audit@example.com", frozenset({"g-aud"}), False, True)
STRANGER = Identity("u-5", "other@example.com", frozenset({"g-x"}), False, False)


def make_grant(**changes) -> Grant:
    base = Grant(
        id="abcde23456",
        state=State.PENDING,
        rule_id="ssm-prod",
        approval=Approval.MANUAL,
        config_version=3,
        requester=Principal(user_id="u-1", user_name="user@example.com"),
        approver_group=LEADS,
        source_ps=PermissionSetRef(arn=PS_ARN, name="ssm-write"),
        account="222222222222",
        duration_seconds=3600,
        reason="incident 42",
        privileged=True,
        created_at="2026-11-15T09:59:00Z",
        task_token=TOKEN,
    )
    return dataclasses.replace(base, **changes)


class FakeTable:
    def __init__(self, *grants):
        self.grants = {g.id: g for g in grants}
        self.transitions = []
        self.queries = []
        self.discarded = []
        self.rows = {}

    def _lock(self, g):
        return (g.requester.user_id, g.source_ps.arn, g.account)

    def put_new(self, grant):
        locks = {self._lock(g) for g in self.grants.values() if g.state != State.CLOSED}
        if self._lock(grant) in locks:
            raise Duplicate(grant.id)
        self.grants[grant.id] = grant

    def discard_new(self, grant):
        if self.grants.get(grant.id) != grant:
            raise StateConflict(grant.id)
        del self.grants[grant.id]
        self.discarded.append(grant.id)

    def get(self, grant_id):
        return self.grants.get(grant_id)

    def transition(self, grant_id, src, dst, **attrs):
        if not can_transition(src, dst):
            raise ValueError(f"{src} -> {dst}")
        grant = self.grants.get(grant_id)
        if grant is None or grant.state != src:
            raise StateConflict(grant_id)
        extra = {"closed_at": iso(NOW), "task_token": None} if dst == State.CLOSED else {}
        self.grants[grant_id] = dataclasses.replace(grant, state=dst, **attrs, **extra)
        self.transitions.append((grant_id, src, dst, attrs))

    def timeline(self, grant_id):
        return self.rows.get(grant_id, [])

    def count_open(self, user_id):
        return sum(
            1
            for g in self.grants.values()
            if g.state != State.CLOSED and g.requester.user_id == user_id
        )

    def _paged(self, grants, limit, after):
        grants = sorted(grants, key=lambda g: (g.created_at, g.id), reverse=True)
        start = after["i"] if after else 0
        page = grants[start : start + limit]
        nxt = {"i": start + limit} if start + limit < len(grants) else None
        return page, nxt

    def by_user(self, user_id, start, end, limit, after, status=None):
        self.queries.append(("by_user", user_id, start, end, status))
        found = [
            g
            for g in self.grants.values()
            if g.requester.user_id == user_id and start <= g.created_at <= end
            if _has_status(g, status)
        ]
        return self._paged(found, limit, after)

    def by_month(self, month, start, end, limit, after, status=None):
        self.queries.append(("by_month", month, start, end, status))
        found = [
            g
            for g in self.grants.values()
            if g.created_at[:7] == month and start <= g.created_at <= end
            if _has_status(g, status)
        ]
        return self._paged(found, limit, after)

    def open_grants(self, limit, after):
        self.queries.append(("open",))
        found = [g for g in self.grants.values() if g.state != State.CLOSED]
        return self._paged(found, limit, after)


def _has_status(grant, status):
    closed = grant.state == State.CLOSED
    return status is None or (status == "closed") == closed


class FakeConfig:
    def __init__(self, *rules, version=3, records=(), docs=None, unavailable=False):
        self.number = version
        self.rules = list(rules) if rules else [AUTO_RULE, MANUAL_RULE]
        self.records = list(records)
        self.docs = docs or {}
        self.unavailable = unavailable

    def current(self):
        if self.unavailable:
            raise ConfigUnavailable("no valid rules version")
        return self.number, self.rules

    def versions(self):
        return self.records

    def version(self, number):
        for record in self.records:
            if record.number == number:
                return record, self.docs.get(number)
        return None


def config_version(number, **changes) -> ConfigVersion:
    return dataclasses.replace(ConfigVersion(number=number, valid=True), **changes)


def source_ps(arn=PS_ARN, **changes) -> SourcePs:
    base = SourcePs(
        arn=arn,
        name="ssm-write",
        managed=("arn:aws:iam::aws:policy/ReadOnlyAccess",),
        customer_managed=({"Name": "team-boundary", "Path": "/"},),
        boundary={"ManagedPolicyArn": "arn:aws:iam::aws:policy/PowerUserAccess"},
        inline={
            "Version": "2012-10-17",
            "Statement": [{"Effect": "Allow", "Action": "ssm:*", "Resource": "*"}],
        },
    )
    return dataclasses.replace(base, **changes)


class FakeSso:
    """Read-only permission set reads, like adapters.sso_read."""

    def __init__(self, *sources):
        self.sources = {s.arn: s for s in (sources or (source_ps(),))}
        self.reads = []

    def read_source(self, arn):
        self.reads.append(arn)
        if arn not in self.sources:
            raise PsNotFound(arn)
        return self.sources[arn]


class FakeSfn:
    def __init__(self, delivered=True, redrivable=True, start_error=None):
        self.started = []
        self.sent = []
        self.redriven = []
        self.delivered = delivered
        self.redrivable = redrivable
        self.start_error = start_error

    def start(self, grant_id, approval):
        if self.start_error:
            raise self.start_error
        self.started.append((grant_id, approval))

    def send_success(self, token, output):
        self.sent.append((token, output))
        return self.delivered

    def redrive(self, grant_id):
        self.redriven.append(grant_id)
        return self.redrivable


class FakeStore:
    def __init__(self, groups=None):
        self.groups = groups if groups is not None else {"u-1": frozenset({"g-req"})}

    def group_ids(self, user_id):
        return self.groups.get(user_id, frozenset())


def assert_same_shape(example, actual, where="$"):
    """Same keys at every level and same JSON types; null on either side matches any
    type, because the example shows a nullable field filled in."""
    if example is None or actual is None:
        return
    if isinstance(example, dict):
        assert isinstance(actual, dict), where
        assert set(actual) == set(example), (where, sorted(set(actual) ^ set(example)))
        for key, value in example.items():
            assert_same_shape(value, actual[key], f"{where}.{key}")
    elif isinstance(example, list):
        assert isinstance(actual, list), where
        for i, item in enumerate(actual):
            # Items are matched by position, the last example item standing for the rest.
            assert example, f"{where}: the example list is empty"
            assert_same_shape(example[min(i, len(example) - 1)], item, f"{where}[{i}]")
    else:
        assert type(actual) is type(example), (where, type(example), type(actual))


@pytest.fixture
def fx():
    """This module: constants, make_grant and the fake classes."""
    return sys.modules[__name__]
