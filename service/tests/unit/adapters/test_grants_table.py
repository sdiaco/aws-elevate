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
                {"Put": {"TableName": TABLE, "Item": ANY}},
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
