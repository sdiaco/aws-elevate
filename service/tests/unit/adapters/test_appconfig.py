import io
import json

import boto3
import pytest
from botocore.exceptions import ClientError
from botocore.response import StreamingBody
from botocore.stub import Stubber

from aws_elevate.adapters.appconfig import AppConfig, NotJson


def test_get_returns_the_document_of_a_version():
    client = boto3.client("appconfig", region_name="eu-west-1")
    body = json.dumps({"schemaVersion": 1, "rules": []}).encode()
    with Stubber(client) as stub:
        stub.add_response(
            "get_hosted_configuration_version",
            {"Content": StreamingBody(io.BytesIO(body), len(body)), "VersionNumber": 7},
            {"ApplicationId": "app1234", "ConfigurationProfileId": "prof123", "VersionNumber": 7},
        )
        assert AppConfig(client, "app1234", "prof123").get(7) == {"schemaVersion": 1, "rules": []}


APP = {"ApplicationId": "app1234", "ConfigurationProfileId": "prof123"}


@pytest.fixture
def appconfig():
    client = boto3.client("appconfig", region_name="eu-west-1")
    with Stubber(client) as stub:
        yield AppConfig(client, "app1234", "prof123"), stub
        stub.assert_no_pending_responses()


def summary(n):
    return {**APP, "VersionNumber": n}


def test_list_versions_newest_first_across_pages(appconfig):
    ac, stub = appconfig
    stub.add_response(
        "list_hosted_configuration_versions",
        {"Items": [summary(1), summary(3)], "NextToken": "t"},
        APP,
    )
    stub.add_response(
        "list_hosted_configuration_versions", {"Items": [summary(2)]}, {**APP, "NextToken": "t"}
    )
    assert ac.list_versions() == [3, 2, 1]


def test_latest(appconfig):
    ac, stub = appconfig
    stub.add_response("list_hosted_configuration_versions", {"Items": [summary(4)]}, APP)
    body = b'{"schemaVersion": 1, "rules": []}'
    stub.add_response(
        "get_hosted_configuration_version",
        {"Content": StreamingBody(io.BytesIO(body), len(body))},
        {**APP, "VersionNumber": 4},
    )
    assert ac.latest() == (4, {"schemaVersion": 1, "rules": []})


def test_latest_without_versions(appconfig):
    ac, stub = appconfig
    stub.add_response("list_hosted_configuration_versions", {"Items": []}, APP)
    assert ac.latest() is None


def test_get_refuses_content_that_is_not_json(appconfig):
    # AppConfig stores any content: the schema is not applied on creation.
    ac, stub = appconfig
    body = b"not json"
    stub.add_response(
        "get_hosted_configuration_version",
        {"Content": StreamingBody(io.BytesIO(body), len(body))},
        {**APP, "VersionNumber": 2},
    )
    with pytest.raises(NotJson):
        ac.get(2)


def test_validate_true(appconfig):
    ac, stub = appconfig
    stub.add_response("validate_configuration", {}, {**APP, "ConfigurationVersion": "5"})
    assert ac.validate(5) == (True, None)


def test_validate_false_with_the_reason(appconfig):
    ac, stub = appconfig
    stub.add_client_error(
        "validate_configuration",
        service_error_code="BadRequestException",
        service_message="JSON Schema validation failed",
        http_status_code=400,
        expected_params={**APP, "ConfigurationVersion": "5"},
    )
    assert ac.validate(5) == (False, "JSON Schema validation failed")


def test_validate_other_errors_propagate(appconfig):
    ac, stub = appconfig
    stub.add_client_error("validate_configuration", service_error_code="InternalServerException")
    with pytest.raises(ClientError):
        ac.validate(5)
