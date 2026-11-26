import boto3
import pytest
from botocore.exceptions import ClientError
from botocore.stub import Stubber

from aws_elevate.adapters.sns import Topics
from aws_elevate.core.notify import Email

ALERTS = "arn:aws:sns:eu-west-1:111111111111:elevate-alerts"
APPROVALS = "arn:aws:sns:eu-west-1:111111111111:elevate-approvals"


@pytest.fixture
def topics():
    client = boto3.client("sns", region_name="eu-west-1")
    with Stubber(client) as stub:
        yield Topics(client, {"alerts": ALERTS, "approvals": APPROVALS}), stub
        stub.assert_no_pending_responses()


def test_publish_sends_subject_and_body_to_the_topic(topics):
    sns, stub = topics
    stub.add_response(
        "publish",
        {"MessageId": "m"},
        {"TopicArn": APPROVALS, "Subject": "the subject", "Message": "the body"},
    )
    sns.publish(Email("approvals", "the subject", "the body"))


def test_publish_error_propagates(topics):
    sns, stub = topics
    stub.add_client_error("publish", "ThrottledException")
    with pytest.raises(ClientError):
        sns.publish(Email("alerts", "s", "b"))
