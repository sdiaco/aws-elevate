import json

import boto3
import pytest
from botocore.exceptions import ClientError
from botocore.stub import Stubber

from aws_elevate.adapters.stepfunctions import StepFunctions

SM_ARN = "arn:aws:states:eu-west-1:111111111111:stateMachine:elevate-grant"
EXEC_ARN = "arn:aws:states:eu-west-1:111111111111:execution:elevate-grant:abcde23456"
TOKEN = "t" * 40


@pytest.fixture
def sfn():
    client = boto3.client("stepfunctions", region_name="eu-west-1")
    with Stubber(client) as stub:
        yield StepFunctions(client, SM_ARN), stub
        stub.assert_no_pending_responses()


def test_execution_arn_is_derived_from_the_grant_id(sfn):
    adapter, _ = sfn
    assert adapter.execution_arn("abcde23456") == EXEC_ARN


def test_start_names_the_execution_after_the_grant_and_passes_only_id_and_approval(sfn):
    adapter, stub = sfn
    stub.add_response(
        "start_execution",
        {"executionArn": EXEC_ARN, "startDate": "2026-11-15T10:00:00Z"},
        {
            "stateMachineArn": SM_ARN,
            "name": "abcde23456",
            "input": json.dumps({"grantId": "abcde23456", "approval": "manual"}),
        },
    )
    adapter.start("abcde23456", "manual")


def test_start_tolerates_an_execution_that_already_exists(sfn):
    adapter, stub = sfn
    stub.add_client_error("start_execution", service_error_code="ExecutionAlreadyExists")
    adapter.start("abcde23456", "auto")


def test_start_raises_other_errors(sfn):
    adapter, stub = sfn
    stub.add_client_error("start_execution", service_error_code="StateMachineDoesNotExist")
    with pytest.raises(ClientError):
        adapter.start("abcde23456", "auto")


def test_send_success_returns_true_when_delivered(sfn):
    adapter, stub = sfn
    stub.add_response(
        "send_task_success",
        {},
        {"taskToken": TOKEN, "output": json.dumps({"decision": "approved"})},
    )
    assert adapter.send_success(TOKEN, {"decision": "approved"}) is True


@pytest.mark.parametrize("code", ["TaskTimedOut", "TaskDoesNotExist"])
def test_send_success_returns_false_when_the_wait_is_already_over(sfn, code):
    adapter, stub = sfn
    stub.add_client_error("send_task_success", service_error_code=code)
    assert adapter.send_success(TOKEN, {"decision": "approved"}) is False


def test_send_success_raises_other_errors(sfn):
    adapter, stub = sfn
    stub.add_client_error("send_task_success", service_error_code="InvalidToken")
    with pytest.raises(ClientError):
        adapter.send_success(TOKEN, {"decision": "approved"})


def test_status_reads_the_execution(sfn):
    adapter, stub = sfn
    stub.add_response(
        "describe_execution",
        {
            "executionArn": EXEC_ARN,
            "stateMachineArn": SM_ARN,
            "status": "FAILED",
            "startDate": "2026-11-15T10:00:00Z",
        },
        {"executionArn": EXEC_ARN},
    )
    assert adapter.status("abcde23456") == "FAILED"


def test_status_is_none_for_a_missing_execution(sfn):
    adapter, stub = sfn
    stub.add_client_error("describe_execution", service_error_code="ExecutionDoesNotExist")
    assert adapter.status("abcde23456") is None


def test_redrive_resumes_the_execution(sfn):
    adapter, stub = sfn
    stub.add_response(
        "redrive_execution", {"redriveDate": "2026-11-15T10:00:00Z"}, {"executionArn": EXEC_ARN}
    )
    assert adapter.redrive("abcde23456") is True


def test_redrive_returns_false_when_not_redrivable(sfn):
    adapter, stub = sfn
    stub.add_client_error("redrive_execution", service_error_code="ExecutionNotRedrivable")
    assert adapter.redrive("abcde23456") is False
