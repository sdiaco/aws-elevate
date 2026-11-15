import json
from datetime import UTC, datetime

import boto3
import pytest
from botocore.stub import Stubber

from aws_elevate.adapters.cloudtrail import CloudTrail

AUTHOR = "arn:aws:sts::111111111111:assumed-role/AWSReservedSSO_ElevateAdmin_abc/user@example.com"
LOOKUP = {
    "LookupAttributes": [
        {"AttributeKey": "EventName", "AttributeValue": "CreateHostedConfigurationVersion"}
    ]
}


def event(version, app="app1234", profile="prof123", error=None):
    record = {
        "eventTime": "2026-11-15T10:00:00Z",
        "userIdentity": {"arn": AUTHOR},
        "requestParameters": {"applicationId": app, "configurationProfileId": profile},
        "responseElements": None if error else {"versionNumber": version},
    }
    if error:
        record["errorCode"] = error
    return {
        "EventId": f"e-{version}-{app}",
        "EventTime": datetime(2026, 11, 15, 10, tzinfo=UTC),
        "CloudTrailEvent": json.dumps(record),
    }


@pytest.fixture
def trail():
    client = boto3.client("cloudtrail", region_name="eu-west-1")
    with Stubber(client) as stub:
        yield CloudTrail(client, "app1234", "prof123"), stub
        stub.assert_no_pending_responses()


def test_author_of_a_version(trail):
    ct, stub = trail
    stub.add_response(
        "lookup_events",
        {"Events": [event(3, app="other"), event(3, error="ConflictException"), event(3)]},
        LOOKUP,
    )
    assert ct.version_author(3) == (AUTHOR, "2026-11-15T10:00:00Z")


def test_author_found_on_a_later_page(trail):
    ct, stub = trail
    stub.add_response("lookup_events", {"Events": [event(9)], "NextToken": "t"}, LOOKUP)
    stub.add_response("lookup_events", {"Events": [event(3)]}, {**LOOKUP, "NextToken": "t"})
    assert ct.version_author(3) == (AUTHOR, "2026-11-15T10:00:00Z")


def test_author_not_recorded_yet(trail):
    # CloudTrail records with a few minutes of delay (design §7.3).
    ct, stub = trail
    stub.add_response("lookup_events", {"Events": [event(2)]}, LOOKUP)
    assert ct.version_author(3) is None
