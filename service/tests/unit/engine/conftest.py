"""Small fakes behind the engine's dependencies. They keep real lifecycle rules so a test
cannot pass by writing a transition the table would refuse."""

import dataclasses
from datetime import UTC, datetime, timedelta

import pytest

from aws_elevate.adapters.grants_table import StateConflict
from aws_elevate.adapters.sso_read import PsNotFound
from aws_elevate.core.lifecycle import can_transition
from aws_elevate.core.models import (
    Approval,
    Grant,
    PermissionSetRef,
    Principal,
    SourcePs,
    State,
)
from aws_elevate.core.times import iso
from aws_elevate.engine.actions import Engine

START = datetime(2026, 11, 15, 10, 0, 0, tzinfo=UTC)
SRC_ARN = "arn:aws:sso:::permissionSet/ssoins-1/ps-1"
JIT_ARN = "arn:aws:sso:::permissionSet/ssoins-1/ps-jit"
# Management and service accounts (design §7.5); never the grant targets of these tests.
RESERVED = frozenset({"999999999999", "888888888888"})

RULE_DOC = {
    "id": "backend-ssm-prod",
    "group": {"id": "g-1", "name": "backend-devs"},
    "permissionSet": {"arn": SRC_ARN, "name": "ssm-write"},
    "accounts": [{"id": "111111111111", "name": "prod"}],
    "duration": {"defaultSeconds": 3600, "maxSeconds": 14400},
    "approval": "auto",
    "approverGroup": None,
    "privileged": False,
}

SOURCE = SourcePs(
    arn=SRC_ARN,
    name="ssm-write",
    managed=("arn:aws:iam::aws:policy/ReadOnlyAccess",),
    customer_managed=(),
    boundary=None,
    inline={
        "Version": "2012-10-17",
        "Statement": [{"Effect": "Allow", "Action": "ssm:*", "Resource": "*"}],
    },
)


def make_grant(**changes) -> Grant:
    base = Grant(
        id="abcde23456",
        state=State.ACTIVATING,
        rule_id="backend-ssm-prod",
        approval=Approval.AUTO,
        config_version=3,
        requester=Principal(user_id="u-1", user_name="user@example.com"),
        approver_group=None,
        source_ps=PermissionSetRef(arn=SRC_ARN, name="ssm-write"),
        account="111111111111",
        duration_seconds=3600,
        reason="incident 42",
        privileged=False,
        created_at="2026-11-15T09:59:00Z",
    )
    return dataclasses.replace(base, **changes)


class FakeTable:
    def __init__(self, *grants):
        self.grants = {g.id: g for g in grants}
        self.calls = []

    def get(self, grant_id):
        return self.grants.get(grant_id)

    def set_attrs(self, grant_id, state, **attrs):
        self.calls.append(("set_attrs", attrs))
        grant = self.grants.get(grant_id)
        if grant is None or grant.state != state:
            raise StateConflict(grant_id)
        self.grants[grant_id] = dataclasses.replace(grant, **attrs)

    def transition(self, grant_id, src, dst, **attrs):
        self.calls.append(("transition", src, dst, attrs))
        if not can_transition(src, dst):
            raise ValueError(f"{src} -> {dst}")
        grant = self.grants.get(grant_id)
        if grant is None or grant.state != src:
            raise StateConflict(grant_id)
        extra = {"closed_at": iso(START)} if dst == State.CLOSED else {}
        self.grants[grant_id] = dataclasses.replace(grant, state=dst, **attrs, **extra)

    def set_token(self, grant_id, token):
        grant = self.grants.get(grant_id)
        if grant is None or grant.state not in (State.PENDING, State.ACTIVE):
            return False
        self.grants[grant_id] = dataclasses.replace(grant, task_token=token)
        return True


class FakeRules:
    def __init__(self, docs=None):
        self.docs = docs if docs is not None else {3: {"schemaVersion": 1, "rules": [RULE_DOC]}}

    def get(self, version):
        return self.docs[version]


class FakeRead:
    def __init__(self, source=SOURCE, existing=None):
        self.source = source
        self.existing = existing  # ARN returned by find_by_name

    def find_by_name(self, name):
        return self.existing

    def read_source(self, arn):
        if self.source is None:
            raise PsNotFound(arn)
        return self.source


class FakeAdmin:
    """Records calls; `fail` maps a method name to the exception it raises."""

    def __init__(self):
        self.calls = []
        self.fail = {}

    def _call(self, name, *args):
        self.calls.append((name, *args))
        if name in self.fail:
            raise self.fail[name]

    def create_ephemeral(self, name, description, session_duration, tags):
        self._call("create_ephemeral", name, description, session_duration, tags)
        return JIT_ARN

    def configure(self, arn, inline, source):
        self._call("configure", arn, inline, source)

    def assign(self, arn, account, user_id):
        self._call("assign", arn, account, user_id)

    def rewrite_inline(self, arn, account, inline):
        self._call("rewrite_inline", arn, account, inline)

    def unassign(self, arn, account, user_id):
        self._call("unassign", arn, account, user_id)

    def delete(self, arn):
        self._call("delete", arn)

    def names(self):
        return [c[0] for c in self.calls]


class Clock:
    def __init__(self):
        self.now = START

    def __call__(self):
        return self.now

    def advance(self, seconds):
        self.now += timedelta(seconds=seconds)


@pytest.fixture
def clock():
    return Clock()


@pytest.fixture
def admin():
    return FakeAdmin()


@pytest.fixture
def build(clock, admin):
    """build(*grants, rules=..., source=...) -> (engine, table)."""

    def _build(*grants, rules=None, source=SOURCE, existing=None, reserved=RESERVED):
        table = FakeTable(*grants)
        engine = Engine(
            table=table,
            rules=rules or FakeRules(),
            sso_read=FakeRead(source, existing),
            sso_admin=admin,
            reserved_accounts=reserved,
            clock=clock,
        )
        return engine, table

    return _build


@pytest.fixture
def grant():
    """grant(**changes) -> a Grant in `activating` for the default rule."""
    return make_grant


@pytest.fixture
def const():
    return dataclasses.make_dataclass(
        "Const", ["start", "src_arn", "jit_arn", "source", "rule_doc"]
    )(START, SRC_ARN, JIT_ARN, SOURCE, RULE_DOC)
