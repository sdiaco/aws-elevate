"""In-memory fakes behind the grant routes. The table keeps the real lifecycle rules and
offers only the reads the real adapter has, so a route cannot pass with a Scan."""

import dataclasses
import sys
from datetime import UTC, datetime

import pytest

from aws_elevate.adapters.grants_table import Duplicate, StateConflict
from aws_elevate.core.lifecycle import can_transition
from aws_elevate.core.models import (
    Account,
    Approval,
    Grant,
    Group,
    Identity,
    PermissionSetRef,
    Principal,
    Rule,
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

    def by_user(self, user_id, start, end, limit, after, open_only=False):
        self.queries.append(("by_user", user_id, start, end, open_only))
        found = [
            g
            for g in self.grants.values()
            if g.requester.user_id == user_id and start <= g.created_at <= end
            if not open_only or g.state != State.CLOSED
        ]
        return self._paged(found, limit, after)

    def by_month(self, month, start, end, limit, after, open_only=False):
        self.queries.append(("by_month", month, start, end, open_only))
        found = [
            g
            for g in self.grants.values()
            if g.created_at[:7] == month and start <= g.created_at <= end
            if not open_only or g.state != State.CLOSED
        ]
        return self._paged(found, limit, after)

    def open_grants(self, limit, after):
        self.queries.append(("open",))
        found = [g for g in self.grants.values() if g.state != State.CLOSED]
        return self._paged(found, limit, after)


class FakeConfig:
    def __init__(self, *rules, version=3):
        self.version = version
        self.rules = list(rules) if rules else [AUTO_RULE, MANUAL_RULE]

    def current(self):
        return self.version, self.rules


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


@pytest.fixture
def fx():
    """This module: constants, make_grant and the fake classes."""
    return sys.modules[__name__]
