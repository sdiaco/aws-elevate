from datetime import UTC, datetime

import boto3
import pytest
from botocore.stub import ANY, Stubber

from aws_elevate.adapters.grants_table import (
    Duplicate,
    GrantsTable,
    StateConflict,
    from_item,
    timeline_item,
    to_item,
)
from aws_elevate.core.models import (
    Approval,
    CloseReason,
    ConfigVersion,
    Grant,
    Group,
    PermissionSetRef,
    Principal,
    State,
)

NOW = datetime(2026, 11, 15, 10, 0, 0, tzinfo=UTC)
TABLE = "elevate-grants"
PS_ARN = "arn:aws:sso:::permissionSet/ssoins-1/ps-1"

GRANT = Grant(
    id="abcde23456",
    state=State.PENDING,
    rule_id="backend-ssm-prod",
    approval=Approval.MANUAL,
    config_version=3,
    requester=Principal(user_id="u-1", user_name="user@example.com"),
    approver_group=Group(id="g-2", name="leads"),
    source_ps=PermissionSetRef(arn=PS_ARN, name="ssm-write"),
    account="111111111111",
    duration_seconds=3600,
    reason="incident 42",
    privileged=False,
    created_at="2026-11-15T09:59:00Z",
)
LOCK_KEY = {"pk": {"S": f"LOCK#u-1#{PS_ARN}#111111111111"}, "sk": {"S": "LOCK"}}
GRANT_KEY = {"pk": {"S": "GRANT#abcde23456"}, "sk": {"S": "META"}}
# The first timeline row is dated createdAt, so its key is known without reading it.
FIRST_ROW = {
    "pk": {"S": "GRANT#abcde23456"},
    "sk": {"S": "EV#2026-11-15T09:59:00Z#pending"},
    "state": {"S": "pending"},
}


@pytest.fixture
def table():
    client = boto3.client("dynamodb", region_name="eu-west-1")
    with Stubber(client) as stub:
        yield GrantsTable(client, TABLE, clock=lambda: NOW), stub
        stub.assert_no_pending_responses()


def cancelled(*codes):
    return {
        "service_error_code": "TransactionCanceledException",
        "service_message": "Transaction cancelled",
        "http_status_code": 400,
        "response_meta": {},
        "service_error_meta": {},
        # botocore puts CancellationReasons in the parsed error; Stubber passes it via
        # modeled_fields.
        "modeled_fields": {"CancellationReasons": [{"Code": c} for c in codes]},
    }


def add_error(stub, method, error):
    stub.add_client_error(method, **error)


def test_item_round_trip_keeps_every_field():
    grant = GRANT.__class__(
        **{
            **GRANT.__dict__,
            "state": State.CLOSED,
            "close_reason": CloseReason.REVOKED,
            "approver": Principal(user_id="u-2", user_name="lead@example.com"),
            "revoked_by": Principal(user_id="u-2", user_name="lead@example.com"),
            "revoke_reason": "done",
            "reject_reason": "not needed",
            "activated_at": "2026-11-15T10:00:00Z",
            "expires_at": "2026-11-15T11:00:00Z",
            "closed_at": "2026-11-15T10:30:00Z",
            "task_token": "tok",
            "error": "boom",
            "ephemeral_ps_arn": "arn:aws:sso:::permissionSet/ssoins-1/ps-2",
        }
    )
    assert from_item(to_item(grant)) == grant


def test_item_has_index_keys_and_open_marker():
    item = to_item(GRANT)
    assert item["pk"] == {"S": "GRANT#abcde23456"}
    assert item["gsi1pk"] == {"S": "USER#u-1"}
    assert item["gsi2pk"] == {"S": "MONTH#2026-11"}
    assert item["gsi2sk"] == {"S": "2026-11-15T09:59:00Z#abcde23456"}
    assert item["gsi3pk"] == {"S": "OPEN"}
    assert "taskToken" not in item


def test_put_new_writes_grant_lock_and_timeline(table):
    gt, stub = table
    stub.add_response(
        "transact_write_items",
        {},
        {
            "TransactItems": [
                {
                    "Put": {
                        "TableName": TABLE,
                        "Item": to_item(GRANT),
                        "ConditionExpression": "attribute_not_exists(pk)",
                    }
                },
                {
                    "Put": {
                        "TableName": TABLE,
                        "Item": {**LOCK_KEY, "grantId": {"S": "abcde23456"}},
                        "ConditionExpression": "attribute_not_exists(pk)",
                    }
                },
                {"Put": {"TableName": TABLE, "Item": FIRST_ROW}},
            ]
        },
    )
    gt.put_new(GRANT)


def test_put_new_with_existing_lock_raises_duplicate(table):
    gt, stub = table
    add_error(stub, "transact_write_items", cancelled("None", "ConditionalCheckFailed", "None"))
    with pytest.raises(Duplicate):
        gt.put_new(GRANT)


def test_get_returns_grant_or_none(table):
    gt, stub = table
    stub.add_response(
        "get_item",
        {"Item": to_item(GRANT)},
        {"TableName": TABLE, "Key": GRANT_KEY, "ConsistentRead": True},
    )
    stub.add_response("get_item", {}, {"TableName": TABLE, "Key": ANY, "ConsistentRead": True})
    assert gt.get("abcde23456") == GRANT
    assert gt.get("missing0000") is None


def test_transition_is_conditional_on_source_state(table):
    gt, stub = table
    stub.add_response(
        "transact_write_items",
        {},
        {
            "TransactItems": [
                {
                    "Update": {
                        "TableName": TABLE,
                        "Key": GRANT_KEY,
                        "UpdateExpression": "SET #state = :dst, #approver = :a0",
                        "ConditionExpression": "#state = :src",
                        "ExpressionAttributeNames": {"#state": "state", "#approver": "approver"},
                        "ExpressionAttributeValues": {
                            ":dst": {"S": "activating"},
                            ":src": {"S": "pending"},
                            ":a0": {
                                "M": {
                                    "userId": {"S": "u-2"},
                                    "userName": {"S": "lead@example.com"},
                                }
                            },
                        },
                    }
                },
                {"Put": {"TableName": TABLE, "Item": ANY}},
            ]
        },
    )
    gt.transition(
        "abcde23456",
        State.PENDING,
        State.ACTIVATING,
        approver=Principal(user_id="u-2", user_name="lead@example.com"),
    )


def test_transition_from_wrong_state_raises_state_conflict(table):
    gt, stub = table
    add_error(stub, "transact_write_items", cancelled("ConditionalCheckFailed", "None"))
    with pytest.raises(StateConflict):
        gt.transition("abcde23456", State.PENDING, State.ACTIVATING)


def test_transition_not_in_lifecycle_is_refused_before_writing(table):
    gt, _ = table
    with pytest.raises(ValueError):
        gt.transition("abcde23456", State.CLOSED, State.ACTIVE)


def test_closing_sets_ttl_removes_open_marker_and_deletes_lock(table):
    gt, stub = table
    stub.add_response(
        "get_item",
        {"Item": to_item(GRANT)},
        {"TableName": TABLE, "Key": GRANT_KEY, "ConsistentRead": True},
    )
    closed_ttl = int(datetime(2027, 12, 15, 10, 0, tzinfo=UTC).timestamp())
    stub.add_response(
        "transact_write_items",
        {},
        {
            "TransactItems": [
                {
                    "Update": {
                        "TableName": TABLE,
                        "Key": GRANT_KEY,
                        "UpdateExpression": (
                            "SET #state = :dst, #closeReason = :a0, #closedAt = :closedAt, "
                            "#ttl = :ttl REMOVE gsi3pk, gsi3sk, taskToken"
                        ),
                        "ConditionExpression": "#state = :src",
                        "ExpressionAttributeNames": {
                            "#state": "state",
                            "#closeReason": "closeReason",
                            "#closedAt": "closedAt",
                            "#ttl": "ttl",
                        },
                        "ExpressionAttributeValues": {
                            ":dst": {"S": "closed"},
                            ":src": {"S": "pending"},
                            ":a0": {"S": "rejected"},
                            ":closedAt": {"S": "2026-11-15T10:00:00Z"},
                            ":ttl": {"N": str(closed_ttl)},
                        },
                    }
                },
                {"Put": {"TableName": TABLE, "Item": ANY}},
                {"Delete": {"TableName": TABLE, "Key": LOCK_KEY}},
            ]
        },
    )
    gt.transition("abcde23456", State.PENDING, State.CLOSED, close_reason=CloseReason.REJECTED)


def test_closing_a_missing_grant_raises_state_conflict(table):
    gt, stub = table
    stub.add_response("get_item", {}, {"TableName": TABLE, "Key": ANY, "ConsistentRead": True})
    with pytest.raises(StateConflict):
        gt.transition("abcde23456", State.PENDING, State.CLOSED, close_reason=CloseReason.REJECTED)


def test_timeline_row_records_state_and_attributes():
    row = timeline_item("abcde23456", State.ACTIVE, NOW, {"expires_at": "2026-11-15T11:00:00Z"})
    assert row["pk"] == {"S": "GRANT#abcde23456"}
    assert row["sk"] == {"S": "EV#2026-11-15T10:00:00Z#active"}
    assert row["state"] == {"S": "active"}
    assert row["expiresAt"] == {"S": "2026-11-15T11:00:00Z"}


def test_set_token_only_while_waiting(table):
    gt, stub = table
    stub.add_response(
        "update_item",
        {},
        {
            "TableName": TABLE,
            "Key": GRANT_KEY,
            "UpdateExpression": "SET taskToken = :t",
            "ConditionExpression": "#state IN (:pending, :active)",
            "ExpressionAttributeNames": {"#state": "state"},
            "ExpressionAttributeValues": {
                ":t": {"S": "tok"},
                ":pending": {"S": "pending"},
                ":active": {"S": "active"},
            },
        },
    )
    stub.add_client_error("update_item", service_error_code="ConditionalCheckFailedException")
    assert gt.set_token("abcde23456", "tok") is True
    assert gt.set_token("abcde23456", "tok") is False


def test_other_errors_propagate(table):
    gt, stub = table
    stub.add_client_error("transact_write_items", service_error_code="ValidationException")
    with pytest.raises(Exception) as info:
        gt.put_new(GRANT)
    assert not isinstance(info.value, Duplicate | StateConflict)


def test_set_attrs_is_conditional_and_writes_no_timeline(table):
    gt, stub = table
    stub.add_response(
        "update_item",
        {},
        {
            "TableName": TABLE,
            "Key": GRANT_KEY,
            "UpdateExpression": "SET #activatedAt = :a0",
            "ConditionExpression": "#state = :state",
            "ExpressionAttributeNames": {"#state": "state", "#activatedAt": "activatedAt"},
            "ExpressionAttributeValues": {
                ":state": {"S": "activating"},
                ":a0": {"S": "2026-11-15T10:00:00Z"},
            },
        },
    )
    gt.set_attrs("abcde23456", State.ACTIVATING, activated_at="2026-11-15T10:00:00Z")


def test_set_attrs_in_other_state_raises_state_conflict(table):
    gt, stub = table
    stub.add_client_error("update_item", service_error_code="ConditionalCheckFailedException")
    with pytest.raises(StateConflict):
        gt.set_attrs("abcde23456", State.ACTIVATING, error="boom")


def test_item_round_trip_of_auto_grant_without_approver_group():
    # Found by the e2e run: absent optional attributes must read back as None.
    grant = GRANT.__class__(**{**GRANT.__dict__, "approval": Approval.AUTO, "approver_group": None})
    assert "approverGroup" not in to_item(grant)
    assert from_item(to_item(grant)) == grant


# ---------- config versions (design §7.1, §7.3) ----------

CONFIG_KEY = {"pk": {"S": "CONFIG"}, "sk": {"S": "V#00000007"}}
INVALID = ConfigVersion(
    number=7, valid=False, errors=("rules[0].approval: must be auto or manual",)
)
INVALID_ITEM = {
    **CONFIG_KEY,
    "version": {"N": "7"},
    "valid": {"BOOL": False},
    "errors": {"L": [{"S": "rules[0].approval: must be auto or manual"}]},
    "verified": {"BOOL": False},
    "rejectedEventSent": {"BOOL": False},
}


def test_get_config_missing(table):
    t, stub = table
    stub.add_response(
        "get_item", {}, {"TableName": TABLE, "Key": CONFIG_KEY, "ConsistentRead": True}
    )
    assert t.get_config(7) is None


def test_get_config_with_author(table):
    t, stub = table
    item = {
        **INVALID_ITEM,
        "author": {"S": "arn:aws:sts::111111111111:assumed-role/r/user@example.com"},
        "createdAt": {"S": "2026-11-15T10:00:00Z"},
        "verified": {"BOOL": True},
    }
    stub.add_response(
        "get_item", {"Item": item}, {"TableName": TABLE, "Key": CONFIG_KEY, "ConsistentRead": True}
    )
    got = t.get_config(7)
    assert got.author.endswith("/user@example.com")
    assert (got.created_at, got.verified, got.valid) == ("2026-11-15T10:00:00Z", True, False)


def test_put_config_first_seen(table):
    t, stub = table
    stub.add_response(
        "put_item",
        {},
        {
            "TableName": TABLE,
            "Item": INVALID_ITEM,
            "ConditionExpression": "attribute_not_exists(pk)",
        },
    )
    assert t.put_config(INVALID) is True


def test_put_config_already_recorded(table):
    t, stub = table
    stub.add_client_error("put_item", service_error_code="ConditionalCheckFailedException")
    assert t.put_config(INVALID) is False


def test_set_config_author(table):
    t, stub = table
    stub.add_response(
        "update_item",
        {},
        {
            "TableName": TABLE,
            "Key": CONFIG_KEY,
            "UpdateExpression": "SET author = :a, createdAt = :c, verified = :t",
            "ExpressionAttributeValues": {
                ":a": {"S": "arn:x"},
                ":c": {"S": "2026-11-15T10:00:00Z"},
                ":t": {"BOOL": True},
            },
        },
    )
    t.set_config_author(7, "arn:x", "2026-11-15T10:00:00Z")


def test_claim_rejected_event_once(table):
    t, stub = table
    claim = {
        "TableName": TABLE,
        "Key": CONFIG_KEY,
        "UpdateExpression": "SET rejectedEventSent = :t",
        "ConditionExpression": "rejectedEventSent = :f",
        "ExpressionAttributeValues": {":t": {"BOOL": True}, ":f": {"BOOL": False}},
    }
    stub.add_response("update_item", {}, claim)
    stub.add_client_error("update_item", service_error_code="ConditionalCheckFailedException")
    assert t.claim_rejected_event(7) is True
    assert t.claim_rejected_event(7) is False


def test_release_rejected_event(table):
    t, stub = table
    stub.add_response(
        "update_item",
        {},
        {
            "TableName": TABLE,
            "Key": CONFIG_KEY,
            "UpdateExpression": "SET rejectedEventSent = :f",
            "ExpressionAttributeValues": {":f": {"BOOL": False}},
        },
    )
    t.release_rejected_event(7)


# ---------- reads by index (design §11.3: never a Scan) ----------


def _query(stub, params, items, last=None):
    resp = {"Items": items}
    if last:
        resp["LastEvaluatedKey"] = last
    stub.add_response("query", resp, {"TableName": TABLE, **params})


def test_by_user_queries_the_user_index_newest_first(table):
    gt, stub = table
    last = {"pk": {"S": "x"}}
    _query(
        stub,
        {
            "IndexName": "byUser",
            "KeyConditionExpression": "gsi1pk = :pk AND gsi1sk BETWEEN :from AND :to",
            "ExpressionAttributeValues": {
                ":pk": {"S": "USER#u-1"},
                ":from": {"S": "2026-10-16T00:00:00Z"},
                ":to": {"S": "2026-11-15T10:00:00Z"},
            },
            "ScanIndexForward": False,
            "Limit": 50,
        },
        [to_item(GRANT)],
        last,
    )
    grants, nxt = gt.by_user("u-1", "2026-10-16T00:00:00Z", "2026-11-15T10:00:00Z", 50, None)
    assert grants == [GRANT]
    assert nxt == last


def test_by_user_open_only_filters_closed_grants_and_continues_from_a_key(table):
    gt, stub = table
    after = {"pk": {"S": "x"}}
    _query(
        stub,
        {
            "IndexName": "byUser",
            "KeyConditionExpression": "gsi1pk = :pk AND gsi1sk BETWEEN :from AND :to",
            "FilterExpression": "#state <> :closed",
            "ExpressionAttributeNames": {"#state": "state"},
            "ExpressionAttributeValues": {
                ":pk": {"S": "USER#u-1"},
                ":from": {"S": "a"},
                ":to": {"S": "b"},
                ":closed": {"S": "closed"},
            },
            "ScanIndexForward": False,
            "Limit": 10,
            "ExclusiveStartKey": after,
        },
        [],
    )
    assert gt.by_user("u-1", "a", "b", 10, after, status="open") == ([], None)


def test_by_month_closed_keeps_only_closed_grants(table):
    gt, stub = table
    _query(
        stub,
        {
            "IndexName": "byMonth",
            "KeyConditionExpression": "gsi2pk = :pk AND gsi2sk BETWEEN :from AND :to",
            "FilterExpression": "#state = :closed",
            "ExpressionAttributeNames": {"#state": "state"},
            "ExpressionAttributeValues": {
                ":pk": {"S": "MONTH#2026-11"},
                ":from": {"S": "a"},
                ":to": {"S": "b#~"},
                ":closed": {"S": "closed"},
            },
            "ScanIndexForward": False,
            "Limit": 10,
        },
        [],
    )
    assert gt.by_month("2026-11", "a", "b", 10, None, status="closed") == ([], None)


def test_by_month_queries_one_month_partition(table):
    gt, stub = table
    _query(
        stub,
        {
            "IndexName": "byMonth",
            "KeyConditionExpression": "gsi2pk = :pk AND gsi2sk BETWEEN :from AND :to",
            "ExpressionAttributeValues": {
                ":pk": {"S": "MONTH#2026-11"},
                ":from": {"S": "2026-11-01T00:00:00Z"},
                # The sort key is <createdAt>#<id>: '~' sorts after every id character.
                ":to": {"S": "2026-11-15T10:00:00Z#~"},
            },
            "ScanIndexForward": False,
            "Limit": 50,
        },
        [to_item(GRANT)],
    )
    grants, nxt = gt.by_month("2026-11", "2026-11-01T00:00:00Z", "2026-11-15T10:00:00Z", 50, None)
    assert grants == [GRANT] and nxt is None


def test_open_grants_reads_the_sparse_open_index(table):
    gt, stub = table
    _query(
        stub,
        {
            "IndexName": "open",
            "KeyConditionExpression": "gsi3pk = :pk",
            "ExpressionAttributeValues": {":pk": {"S": "OPEN"}},
            "ScanIndexForward": False,
            "Limit": 50,
        },
        [to_item(GRANT)],
    )
    assert gt.open_grants(50, None) == ([GRANT], None)


def test_count_open_counts_the_user_grants_on_every_page_of_the_open_index(table):
    gt, stub = table
    params = {
        "IndexName": "open",
        "KeyConditionExpression": "gsi3pk = :pk",
        "FilterExpression": "requester.userId = :u",
        "ExpressionAttributeValues": {":pk": {"S": "OPEN"}, ":u": {"S": "u-1"}},
        "Select": "COUNT",
    }
    last = {"pk": {"S": "x"}}
    stub.add_response(
        "query", {"Count": 2, "LastEvaluatedKey": last}, {"TableName": TABLE, **params}
    )
    stub.add_response(
        "query", {"Count": 1}, {"TableName": TABLE, **params, "ExclusiveStartKey": last}
    )
    assert gt.count_open("u-1") == 3


def test_timeline_returns_rows_oldest_first_without_the_token(table):
    gt, stub = table
    approver = Principal(user_id="u-2", user_name="lead@example.com")
    rows = [
        FIRST_ROW,
        timeline_item(
            "abcde23456",
            State.ACTIVATING,
            NOW,
            {"approver": approver, "task_token": "secret"},
        ),
    ]
    stub.add_response(
        "query",
        {"Items": rows, "LastEvaluatedKey": {"pk": {"S": "x"}}},
        {
            "TableName": TABLE,
            "KeyConditionExpression": "pk = :pk AND begins_with(sk, :ev)",
            "ExpressionAttributeValues": {":pk": {"S": "GRANT#abcde23456"}, ":ev": {"S": "EV#"}},
            "ConsistentRead": True,
        },
    )
    stub.add_response(
        "query",
        {"Items": []},
        {
            "TableName": TABLE,
            **{},
            "KeyConditionExpression": ANY,
            "ExpressionAttributeValues": ANY,
            "ConsistentRead": True,
            "ExclusiveStartKey": {"pk": {"S": "x"}},
        },
    )
    assert gt.timeline("abcde23456") == [
        {"at": "2026-11-15T09:59:00Z", "state": "pending"},
        {
            "at": "2026-11-15T10:00:00Z",
            "state": "activating",
            "approver": {"userId": "u-2", "userName": "lead@example.com"},
        },
    ]


def test_discard_new_removes_grant_lock_and_first_row_while_untouched(table):
    gt, stub = table
    stub.add_response(
        "transact_write_items",
        {},
        {
            "TransactItems": [
                {
                    "Delete": {
                        "TableName": TABLE,
                        "Key": GRANT_KEY,
                        "ConditionExpression": "#state = :state",
                        "ExpressionAttributeNames": {"#state": "state"},
                        "ExpressionAttributeValues": {":state": {"S": "pending"}},
                    }
                },
                {"Delete": {"TableName": TABLE, "Key": LOCK_KEY}},
                {
                    "Delete": {
                        "TableName": TABLE,
                        "Key": {"pk": FIRST_ROW["pk"], "sk": FIRST_ROW["sk"]},
                    }
                },
            ]
        },
    )
    gt.discard_new(GRANT)


def test_discard_new_raises_state_conflict_once_the_grant_moved(table):
    gt, stub = table
    add_error(stub, "transact_write_items", cancelled("ConditionalCheckFailed", "None", "None"))
    with pytest.raises(StateConflict):
        gt.discard_new(GRANT)
