import io
import json

import boto3
from botocore.response import StreamingBody
from botocore.stub import Stubber

from aws_elevate.adapters.appconfig import AppConfig


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
