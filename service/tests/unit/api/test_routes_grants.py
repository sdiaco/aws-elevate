import base64
import dataclasses
import json
from datetime import timedelta

import pytest
from botocore.exceptions import ClientError

from aws_elevate.api.errors import ApiError
from aws_elevate.api.routes_grants import GrantRoutes
from aws_elevate.core.models import CloseReason, Principal, State
from aws_elevate.core.times import parse_iso


@pytest.fixture
def make(fx):
    def build(*grants, config=None, sfn=None, store=None, max_open=5):
        table = fx.FakeTable(*grants)
        sfn = sfn or fx.FakeSfn()
        routes = GrantRoutes(
            table=table,
            config=config or fx.FakeConfig(),
            sfn=sfn,
            store=store or fx.FakeStore(),
            max_open=max_open,
            clock=lambda: fx.NOW,
        )
        return routes, table, sfn

    return build


def error_of(call) -> ApiError:
    with pytest.raises(ApiError) as caught:
        call()
    return caught.value


def assert_no_token(body):
    text = json.dumps(body)
    assert "taskToken" not in text
    assert "token-" not in text


REQUEST = {"ruleId": "ssm-prod", "account": "222222222222", "durationSeconds": 3600, "reason": "x"}


# ---------- POST /grants (design §6.1) ----------


def test_manual_request_is_recorded_pending_and_started(make, fx):
    routes, table, sfn = make()
    body = routes.create(fx.REQUESTER, REQUEST)
    grant = body["grant"]
    assert grant["state"] == "pending"
    assert grant["approval"] == "manual"
    assert grant["requester"] == {"userId": "u-1", "userName": "user@example.com"}
    assert grant["approverGroup"] == {"id": "g-lead", "name": "leads"}
    assert grant["sourcePs"] == {"arn": fx.PS_ARN, "name": "ssm-write"}
    assert grant["configVersion"] == 3
    assert grant["createdAt"] == "2026-11-15T10:00:00Z"
    assert grant["privileged"] is True
    assert grant["ruleRemoved"] is False
    # The execution stores the wait's token a moment later: until then, no cancel.
    assert (grant["starting"], grant["actions"]) == (True, [])
    assert sfn.started == [(grant["id"], "manual")]
    assert table.get(grant["id"]).state == State.PENDING


def test_auto_request_starts_activating(make, fx):
    routes, _, sfn = make()
    body = routes.create(fx.REQUESTER, {**REQUEST, "ruleId": "ssm-dev", "account": "111111111111"})
    assert body["grant"]["state"] == "activating"
    assert sfn.started == [(body["grant"]["id"], "auto")]


def test_duration_defaults_to_the_rule_default(make, fx):
    routes, _, _ = make()
    request = {k: v for k, v in REQUEST.items() if k != "durationSeconds"}
    assert routes.create(fx.REQUESTER, request)["grant"]["durationSeconds"] == 3600


def test_unknown_rule_is_not_found(make, fx):
    routes, _, _ = make()
    err = error_of(lambda: routes.create(fx.REQUESTER, {**REQUEST, "ruleId": "nope"}))
    assert (err.status, err.code) == (404, "not_found")


def test_rule_of_another_group_is_forbidden(make, fx):
    routes, _, sfn = make()
    err = error_of(lambda: routes.create(fx.STRANGER, REQUEST))
    assert (err.status, err.code) == (403, "forbidden")
    assert sfn.started == []


@pytest.mark.parametrize(
    "change",
    [
        {"account": "333333333333"},
        {"durationSeconds": 99999},
        {"durationSeconds": "3600"},
        {"durationSeconds": True},
        {"reason": "   "},
        {"reason": None},
        {"ruleId": 7},
        {"account": None},
    ],
)
def test_invalid_request_is_refused(make, fx, change):
    routes, table, _ = make()
    err = error_of(lambda: routes.create(fx.REQUESTER, {**REQUEST, **change}))
    assert (err.status, err.code) == (400, "invalid_request")
    assert table.grants == {}


def test_body_that_is_not_an_object_is_refused(make, fx):
    routes, _, _ = make()
    assert error_of(lambda: routes.create(fx.REQUESTER, [])).code == "invalid_request"


def test_reason_is_stored_without_surrounding_spaces(make, fx):
    routes, _, _ = make()
    assert routes.create(fx.REQUESTER, {**REQUEST, "reason": "  fix  "})["grant"]["reason"] == "fix"


def test_same_request_twice_is_a_duplicate(make, fx):
    routes, _, _ = make()
    routes.create(fx.REQUESTER, REQUEST)
    err = error_of(lambda: routes.create(fx.REQUESTER, REQUEST))
    assert (err.status, err.code) == (409, "duplicate")


def test_too_many_open_grants_is_rate_limited(make, fx):
    routes, _, sfn = make(fx.make_grant(account="111111111111"), max_open=1)
    err = error_of(lambda: routes.create(fx.REQUESTER, REQUEST))
    assert (err.status, err.code) == (429, "rate_limited")
    assert sfn.started == []


def test_closed_grants_do_not_count_as_open(make, fx):
    closed = fx.make_grant(state=State.CLOSED, close_reason=CloseReason.EXPIRED)
    routes, _, _ = make(closed, max_open=1)
    assert routes.create(fx.REQUESTER, REQUEST)["grant"]["state"] == "pending"


def test_failed_start_discards_the_grant_and_raises(make, fx):
    failure = ClientError({"Error": {"Code": "AccessDeniedException"}}, "StartExecution")
    routes, table, _ = make(sfn=fx.FakeSfn(start_error=failure))
    with pytest.raises(ClientError):
        routes.create(fx.REQUESTER, REQUEST)
    assert table.grants == {}
    assert len(table.discarded) == 1


# ---------- approve, reject, cancel (design §6.2) ----------


def test_approve_moves_to_activating_and_ends_the_wait(make, fx):
    routes, table, sfn = make(fx.make_grant())
    grant = routes.approve(fx.LEAD, "abcde23456")["grant"]
    assert grant["state"] == "activating"
    assert grant["approver"] == {"userId": "u-2", "userName": "lead@example.com"}
    assert table.transitions == [
        (
            "abcde23456",
            State.PENDING,
            State.ACTIVATING,
            {"approver": Principal("u-2", "lead@example.com")},
        )
    ]
    assert sfn.sent == [(fx.TOKEN, {"decision": "approved"})]
    assert_no_token(grant)


def test_approve_still_succeeds_when_the_wait_already_ended(make, fx):
    # The record wins: the timeout branch reads it and follows the approval.
    routes, _, _ = make(fx.make_grant(), sfn=fx.FakeSfn(delivered=False))
    assert routes.approve(fx.LEAD, "abcde23456")["grant"]["state"] == "activating"


def test_approver_cannot_approve_their_own_request(make, fx):
    own = fx.make_grant(requester=Principal("u-2", "lead@example.com"))
    routes, table, _ = make(own)
    assert error_of(lambda: routes.approve(fx.LEAD, "abcde23456")).code == "forbidden"
    assert table.transitions == []


def test_admin_outside_the_approver_group_cannot_approve(make, fx):
    routes, _, _ = make(fx.make_grant())
    assert error_of(lambda: routes.approve(fx.ADMIN, "abcde23456")).code == "forbidden"


def test_stranger_cannot_see_or_approve(make, fx):
    routes, _, _ = make(fx.make_grant())
    assert error_of(lambda: routes.approve(fx.STRANGER, "abcde23456")).status == 403


def test_missing_grant_is_not_found(make, fx):
    routes, _, _ = make()
    assert error_of(lambda: routes.approve(fx.LEAD, "zzzzz23456")).code == "not_found"


@pytest.mark.parametrize(
    ("changes", "starting"),
    [
        ({"task_token": None}, True),
        ({}, False),
        ({"state": State.ACTIVE, "task_token": None}, False),
    ],
)
def test_a_pending_grant_is_starting_until_its_wait_has_a_token(make, fx, changes, starting):
    routes, _, _ = make(fx.make_grant(**changes))
    assert routes.get(fx.LEAD, "abcde23456")["grant"]["starting"] is starting


def test_approve_before_the_wait_has_its_token_is_not_ready(make, fx):
    routes, table, _ = make(fx.make_grant(task_token=None))
    err = error_of(lambda: routes.approve(fx.LEAD, "abcde23456"))
    assert (err.status, err.code) == (409, "not_ready")
    assert table.transitions == []


def test_approve_after_the_wait_timed_out_is_a_conflict(make, fx):
    timed_out = fx.make_grant(
        state=State.CLOSED, close_reason=CloseReason.TIMED_OUT, task_token=None
    )
    routes, _, _ = make(timed_out)
    err = error_of(lambda: routes.approve(fx.LEAD, "abcde23456"))
    assert (err.status, err.code) == (409, "state_conflict")


def test_approve_that_loses_the_race_is_a_conflict(make, fx):
    routes, table, sfn = make(fx.make_grant())

    def lose(*args, **kwargs):
        raise fx.StateConflict("abcde23456")

    table.transition = lose
    assert error_of(lambda: routes.approve(fx.LEAD, "abcde23456")).code == "state_conflict"
    assert sfn.sent == []


def test_approve_on_a_removed_rule_invalidates(make, fx):
    routes, table, sfn = make(fx.make_grant(), config=fx.FakeConfig(fx.AUTO_RULE))
    grant = routes.approve(fx.LEAD, "abcde23456")["grant"]
    assert (grant["state"], grant["closeReason"]) == ("closed", "invalidated")
    assert grant["ruleRemoved"] is True
    assert "ssm-prod" in grant["error"]
    assert sfn.sent == [(fx.TOKEN, {"decision": "invalidated"})]


def test_approve_on_a_rule_that_no_longer_allows_the_request_invalidates(make, fx):
    shorter = fx.dataclasses.replace(fx.MANUAL_RULE, max_seconds=1800)
    routes, _, _ = make(fx.make_grant(), config=fx.FakeConfig(fx.AUTO_RULE, shorter))
    grant = routes.approve(fx.LEAD, "abcde23456")["grant"]
    assert grant["closeReason"] == "invalidated"
    assert grant["ruleRemoved"] is False


def test_approve_when_the_requester_left_the_group_invalidates(make, fx):
    routes, _, _ = make(fx.make_grant(), store=fx.FakeStore({"u-1": frozenset()}))
    grant = routes.approve(fx.LEAD, "abcde23456")["grant"]
    assert grant["closeReason"] == "invalidated"
    assert "backend-devs" in grant["error"]


def test_reject_closes_and_records_the_approver(make, fx):
    routes, table, sfn = make(fx.make_grant())
    grant = routes.reject(fx.LEAD, "abcde23456", {"reason": "use the read-only rule"})["grant"]
    assert (grant["state"], grant["closeReason"]) == ("closed", "rejected")
    assert grant["approver"]["userId"] == "u-2"
    assert grant["rejectReason"] == "use the read-only rule"
    assert sfn.sent == [(fx.TOKEN, {"decision": "rejected"})]


@pytest.mark.parametrize("body", [None, {}, {"reason": "  "}, {"reason": "r" * 501}])
def test_reject_needs_a_reason(make, fx, body):
    # The requester reads why in the panel, in History and in the rejected event.
    routes, table, sfn = make(fx.make_grant())
    err = error_of(lambda: routes.reject(fx.LEAD, "abcde23456", body))
    assert err.code == "invalid_request"
    assert (table.transitions, sfn.sent) == ([], [])


def test_requester_can_cancel_but_not_approve(make, fx):
    routes, _, sfn = make(fx.make_grant())
    assert error_of(lambda: routes.approve(fx.REQUESTER, "abcde23456")).code == "forbidden"
    grant = routes.cancel(fx.REQUESTER, "abcde23456")["grant"]
    assert grant["closeReason"] == "cancelled"
    assert sfn.sent == [(fx.TOKEN, {"decision": "cancelled"})]


def test_approver_cannot_cancel(make, fx):
    routes, _, _ = make(fx.make_grant())
    assert error_of(lambda: routes.cancel(fx.LEAD, "abcde23456")).code == "forbidden"


# ---------- revoke (design §6.4) ----------

ACTIVE = {
    "state": State.ACTIVE,
    "activated_at": "2026-11-15T09:59:30Z",
    "expires_at": "2026-11-15T10:59:30Z",
}


def test_requester_revokes_an_active_grant(make, fx):
    routes, table, sfn = make(fx.make_grant(**ACTIVE))
    grant = routes.revoke(fx.REQUESTER, "abcde23456", {"reason": " done "})["grant"]
    assert grant["state"] == "revoking"
    assert grant["revokedBy"] == {"userId": "u-1", "userName": "user@example.com"}
    assert grant["revokeReason"] == "done"
    assert sfn.sent == [(fx.TOKEN, {"decision": "revoked"})]


@pytest.mark.parametrize("who", ["LEAD", "ADMIN"])
def test_approver_and_admin_revoke(make, fx, who):
    routes, _, _ = make(fx.make_grant(**ACTIVE))
    grant = routes.revoke(getattr(fx, who), "abcde23456", {"reason": "r"})["grant"]
    assert grant["state"] == "revoking"


def test_revoke_while_activating_says_retry(make, fx):
    routes, table, _ = make(fx.make_grant(state=State.ACTIVATING))
    err = error_of(lambda: routes.revoke(fx.REQUESTER, "abcde23456", {"reason": "r"}))
    assert (err.status, err.code) == (409, "activating")
    assert table.transitions == []


def test_revoke_before_the_active_wait_has_its_token_is_not_ready(make, fx):
    routes, _, _ = make(fx.make_grant(**ACTIVE, task_token=None))
    err = error_of(lambda: routes.revoke(fx.REQUESTER, "abcde23456", {"reason": "r"}))
    assert err.code == "not_ready"


@pytest.mark.parametrize("body", [{}, {"reason": ""}, {"reason": 3}, None])
def test_revoke_needs_a_reason(make, fx, body):
    routes, _, _ = make(fx.make_grant(**ACTIVE))
    assert error_of(lambda: routes.revoke(fx.REQUESTER, "abcde23456", body)).status == 400


def test_revoke_of_a_closed_grant_is_a_conflict(make, fx):
    closed = fx.make_grant(state=State.CLOSED, close_reason=CloseReason.EXPIRED)
    routes, _, _ = make(closed)
    err = error_of(lambda: routes.revoke(fx.REQUESTER, "abcde23456", {"reason": "r"}))
    assert err.code == "state_conflict"


def test_auditor_cannot_revoke(make, fx):
    routes, _, _ = make(fx.make_grant(**ACTIVE))
    err = error_of(lambda: routes.revoke(fx.AUDITOR, "abcde23456", {"reason": "r"}))
    assert err.code == "forbidden"


# ---------- redrive (design §6.5) ----------


def test_admin_redrives_a_failed_revocation(make, fx):
    routes, _, sfn = make(fx.make_grant(state=State.REVOKE_FAILED, **{"error": "boom"}))
    grant = routes.redrive(fx.ADMIN, "abcde23456")["grant"]
    assert grant["state"] == "revoke_failed"
    assert sfn.redriven == ["abcde23456"]


def test_redrive_of_a_revoking_grant_is_a_conflict_even_if_its_execution_failed(make, fx):
    routes, _, sfn = make(fx.make_grant(state=State.REVOKING))
    assert error_of(lambda: routes.redrive(fx.ADMIN, "abcde23456")).code == "state_conflict"
    assert sfn.redriven == []


def test_execution_that_cannot_be_redriven_is_a_conflict(make, fx):
    routes, _, _ = make(fx.make_grant(state=State.REVOKE_FAILED), sfn=fx.FakeSfn(redrivable=False))
    assert error_of(lambda: routes.redrive(fx.ADMIN, "abcde23456")).code == "state_conflict"


@pytest.mark.parametrize("who", ["REQUESTER", "LEAD", "AUDITOR"])
def test_only_admin_redrives(make, fx, who):
    routes, _, sfn = make(fx.make_grant(state=State.REVOKE_FAILED))
    assert error_of(lambda: routes.redrive(getattr(fx, who), "abcde23456")).code == "forbidden"
    assert sfn.redriven == []


# ---------- GET /grants/{id} ----------


def test_get_returns_grant_and_timeline(make, fx):
    routes, table, _ = make(fx.make_grant())
    table.rows["abcde23456"] = [{"at": "2026-11-15T09:59:00Z", "state": "pending"}]
    body = routes.get(fx.AUDITOR, "abcde23456")
    assert body["grant"]["id"] == "abcde23456"
    assert body["grant"]["actions"] == []
    assert body["timeline"] == [{"at": "2026-11-15T09:59:00Z", "state": "pending"}]
    assert_no_token(body)


def test_get_by_a_stranger_is_forbidden(make, fx):
    routes, _, _ = make(fx.make_grant())
    assert error_of(lambda: routes.get(fx.STRANGER, "abcde23456")).code == "forbidden"


def test_get_of_a_missing_grant_is_not_found(make, fx):
    routes, _, _ = make()
    assert error_of(lambda: routes.get(fx.ADMIN, "zzzzz23456")).code == "not_found"


def test_grant_view_has_every_contract_key(make, fx):
    routes, _, _ = make(fx.make_grant())
    assert set(routes.get(fx.LEAD, "abcde23456")["grant"]) == {
        "id", "state", "closeReason", "ruleId", "approval", "configVersion", "requester",
        "approver", "approverGroup", "sourcePs", "account", "durationSeconds", "reason",
        "privileged", "createdAt", "activatedAt", "expiresAt", "closedAt", "revokedBy",
        "revokeReason", "rejectReason", "error", "ruleRemoved", "starting", "actions",
        "approvalExpiresAt",
    }  # fmt: skip


def test_a_pending_grant_says_when_its_approval_wait_ends(make, fx):
    pending = fx.make_grant()
    active = dataclasses.replace(pending, id="bcdef34567", state=State.ACTIVE)
    routes, _, _ = make(pending, active)
    created = parse_iso(pending.created_at)
    view = routes.get(fx.LEAD, "abcde23456")["grant"]
    assert parse_iso(view["approvalExpiresAt"]) == created + timedelta(seconds=1800)
    assert routes.get(fx.LEAD, "bcdef34567")["grant"]["approvalExpiresAt"] is None


# ---------- GET /grants (design §11.3: by key, paginated, never a Scan) ----------


def ids(body):
    return [g["id"] for g in body["items"]]


def g(fx, gid, created, **changes):
    return fx.make_grant(id=gid, created_at=created, account=gid[:12].ljust(12, "0"), **changes)


def test_mine_reads_the_caller_index_over_the_last_30_days(make, fx):
    routes, table, _ = make(
        g(fx, "aaaaa23456", "2026-11-10T00:00:00Z"),
        g(fx, "bbbbb23456", "2026-11-14T00:00:00Z"),
        g(fx, "ccccc23456", "2026-11-14T00:00:00Z", requester=Principal("u-9", "x")),
    )
    body = routes.list(fx.REQUESTER, {"scope": "mine"})
    assert ids(body) == ["bbbbb23456", "aaaaa23456"]
    assert body["cursor"] is None
    assert table.queries == [
        ("by_user", "u-1", "2026-10-16T10:00:00Z", "2026-11-15T10:00:00Z", None)
    ]
    assert_no_token(body)


def test_mine_open_filters_closed(make, fx):
    routes, table, _ = make()
    routes.list(fx.REQUESTER, {"scope": "mine", "open": "true"})
    assert table.queries[0][-1] == "open"


def test_mine_closed_keeps_only_closed_grants(make, fx):
    # History shows closed grants only, filtered here and not in the client (design §11.2).
    routes, table, _ = make(
        g(fx, "aaaaa23456", "2026-11-15T09:00:00Z"),
        g(fx, "bbbbb23456", "2026-11-15T09:30:00Z", state=State.CLOSED),
    )
    body = routes.list(fx.REQUESTER, {"scope": "mine", "closed": "true"})
    assert ids(body) == ["bbbbb23456"]
    assert table.queries[0][-1] == "closed"


def test_all_closed_walks_the_months_with_the_closed_filter(make, fx):
    routes, table, _ = make(
        g(fx, "aaaaa23456", "2026-11-14T00:00:00Z"),
        g(fx, "bbbbb23456", "2026-11-14T01:00:00Z", state=State.CLOSED),
    )
    body = routes.list(fx.AUDITOR, {"scope": "all", "closed": "true"})
    assert ids(body) == ["bbbbb23456"]
    assert {q[-1] for q in table.queries} == {"closed"}


def test_all_closed_for_one_user_reads_that_user_index(make, fx):
    routes, table, _ = make()
    routes.list(fx.ADMIN, {"scope": "all", "user": "u-9", "closed": "true"})
    assert table.queries == [
        ("by_user", "u-9", "2026-10-16T10:00:00Z", "2026-11-15T10:00:00Z", "closed")
    ]


def test_period_is_taken_from_the_query(make, fx):
    routes, table, _ = make()
    routes.list(
        fx.REQUESTER,
        {"scope": "mine", "from": "2026-01-01T00:00:00Z", "to": "2026-02-01T00:00:00Z"},
    )
    assert table.queries[0][2:4] == ("2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z")


@pytest.mark.parametrize(
    "query",
    [
        {"scope": "everything"},
        {"scope": "mine", "from": "yesterday"},
        {"scope": "mine", "from": "2026-11-15T00:00:00Z", "to": "2026-11-01T00:00:00Z"},
        {"scope": "mine", "from": "2025-09-30T00:00:00Z"},  # beyond the 13 months kept
        {"scope": "mine", "open": "yes"},
        {"scope": "mine", "closed": "yes"},
        {"scope": "mine", "open": "true", "closed": "true"},
        {"scope": "approvable", "closed": "true"},
        {"scope": "mine", "user": "u-2"},
        {"scope": "mine", "cursor": "not-a-cursor"},
    ],
)
def test_invalid_list_query_is_refused(make, fx, query):
    routes, _, _ = make()
    assert error_of(lambda: routes.list(fx.REQUESTER, query)).code == "invalid_request"


def test_scope_defaults_to_mine(make, fx):
    routes, table, _ = make()
    routes.list(fx.REQUESTER, {})
    assert table.queries[0][0] == "by_user"


def test_approvable_reads_the_open_index_for_the_caller_groups(make, fx):
    other = fx.Group(id="g-other", name="others")
    routes, table, _ = make(
        g(fx, "aaaaa23456", "2026-11-15T09:00:00Z"),
        g(fx, "bbbbb23456", "2026-11-15T09:30:00Z", approver_group=other),
    )
    body = routes.list(fx.LEAD, {"scope": "approvable"})
    assert ids(body) == ["aaaaa23456"]
    assert body["items"][0]["actions"] == ["approve", "reject"]
    assert table.queries == [("open",)]


def test_approvable_takes_no_user(make, fx):
    routes, _, _ = make()
    err = error_of(lambda: routes.list(fx.LEAD, {"scope": "approvable", "user": "u-9"}))
    assert err.code == "invalid_request"


@pytest.mark.parametrize("who", ["REQUESTER", "LEAD"])
def test_scope_all_needs_admin_or_auditor(make, fx, who):
    routes, _, _ = make()
    assert error_of(lambda: routes.list(getattr(fx, who), {"scope": "all"})).code == "forbidden"


def test_all_walks_the_months_of_the_period_newest_first(make, fx):
    routes, table, _ = make(
        g(fx, "aaaaa23456", "2026-10-20T00:00:00Z"),
        g(fx, "bbbbb23456", "2026-11-14T00:00:00Z"),
    )
    body = routes.list(fx.AUDITOR, {"scope": "all"})
    assert ids(body) == ["bbbbb23456", "aaaaa23456"]
    assert [q[1] for q in table.queries] == ["2026-11", "2026-10"]
    assert body["cursor"] is None


def test_all_pages_of_50_continue_across_months_with_the_cursor(make, fx):
    grants = [g(fx, f"n{i:04d}23456"[:10], f"2026-11-14T00:{i // 60:02d}:{i % 60:02d}Z")
              for i in range(30)]  # fmt: skip
    grants += [g(fx, f"o{i:04d}23456"[:10], f"2026-10-20T00:00:{i:02d}Z") for i in range(30)]
    routes, _, _ = make(*grants)
    first = routes.list(fx.ADMIN, {"scope": "all"})
    assert len(first["items"]) == 50
    assert first["cursor"] is not None
    second = routes.list(fx.ADMIN, {"scope": "all", "cursor": first["cursor"]})
    assert len(second["items"]) == 10
    assert second["cursor"] is None
    assert set(ids(first)) | set(ids(second)) == {x.id for x in grants}


def test_all_for_one_user_reads_that_user_index(make, fx):
    routes, table, _ = make()
    routes.list(fx.ADMIN, {"scope": "all", "user": "u-9"})
    assert table.queries[0][:2] == ("by_user", "u-9")


def test_all_open_reads_the_open_index(make, fx):
    routes, table, _ = make()
    routes.list(fx.ADMIN, {"scope": "all", "open": "true"})
    assert table.queries == [("open",)]


def test_cursor_of_a_month_outside_the_period_is_refused(make, fx):
    routes, _, _ = make()
    cursor = base64.urlsafe_b64encode(json.dumps({"m": "2020-01", "k": None}).encode()).decode()
    err = error_of(lambda: routes.list(fx.ADMIN, {"scope": "all", "cursor": cursor}))
    assert err.code == "invalid_request"


def test_list_never_returns_a_grant_the_caller_cannot_view(make, fx):
    # Second line of defence: whatever an index returns (a tampered cursor, a bug in a
    # key condition), the caller sees only grants they are allowed to view.
    foreign = g(fx, "zzzzz23456", "2026-11-14T00:00:00Z", requester=Principal("u-9", "x"))
    routes, table, _ = make(foreign)
    table.by_user = lambda *args, **kwargs: ([foreign], None)
    assert routes.list(fx.REQUESTER, {"scope": "mine"})["items"] == []


# ---------- reason length (approved by the maintainer: 500 characters) ----------


def test_request_reason_of_500_characters_is_accepted(make, fx):
    routes, _, _ = make()
    assert routes.create(fx.REQUESTER, {**REQUEST, "reason": "r" * 500})["grant"]["reason"]


def test_request_reason_over_500_characters_is_refused(make, fx):
    routes, table, _ = make()
    err = error_of(lambda: routes.create(fx.REQUESTER, {**REQUEST, "reason": "r" * 501}))
    assert (err.status, err.code) == (400, "invalid_request")
    assert "500" in err.message
    assert table.grants == {}


def test_revoke_reason_over_500_characters_is_refused(make, fx):
    routes, table, _ = make(fx.make_grant(**ACTIVE))
    err = error_of(lambda: routes.revoke(fx.REQUESTER, "abcde23456", {"reason": "r" * 501}))
    assert err.code == "invalid_request"
    assert table.transitions == []
