import json

import boto3
import pytest
from botocore.stub import Stubber

from aws_elevate.adapters.events import EventPublishFailed, Events


@pytest.fixture
def bus():
    client = boto3.client("events", region_name="eu-west-1")
    with Stubber(client) as stub:
        yield Events(client, "elevate-events"), stub
        stub.assert_no_pending_responses()


def entry():
    return {
        "Source": "elevate",
        "DetailType": "elevate.config.rejected",
        "EventBusName": "elevate-events",
        "Detail": json.dumps(
            {
                "schemaVersion": 1,
                "eventId": "config:7:rejected",
                "configVersion": 7,
                "author": None,
                "errors": ["rules[0].approval: must be auto or manual"],
            }
        ),
    }


def test_put_config_rejected(bus):
    events, stub = bus
    stub.add_response(
        "put_events", {"FailedEntryCount": 0, "Entries": [{"EventId": "x"}]}, {"Entries": [entry()]}
    )
    events.put_config_rejected(7, None, ["rules[0].approval: must be auto or manual"])


def test_failed_entry_raises(bus):
    events, stub = bus
    stub.add_response(
        "put_events",
        {"FailedEntryCount": 1, "Entries": [{"ErrorCode": "InternalFailure", "ErrorMessage": "x"}]},
    )
    with pytest.raises(EventPublishFailed):
        events.put_config_rejected(7, None, ["rules[0].approval: must be auto or manual"])
