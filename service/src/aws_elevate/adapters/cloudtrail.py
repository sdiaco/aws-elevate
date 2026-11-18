"""Who created a rules version and when. AppConfig does not expose it;
CloudTrail Event history (free, 90 days) does, a few minutes after the fact."""

import json


class CloudTrail:
    def __init__(self, client, application_id: str, profile_id: str):
        self._trail = client
        self._app = application_id
        self._profile = profile_id

    def version_author(self, version: int) -> tuple[str, str] | None:
        """(principal ARN, ISO time) of the call that created `version`, or None if
        CloudTrail has not recorded it yet."""
        args = {
            "LookupAttributes": [
                {"AttributeKey": "EventName", "AttributeValue": "CreateHostedConfigurationVersion"}
            ]
        }
        while True:
            page = self._trail.lookup_events(**args)
            for event in page["Events"]:
                record = json.loads(event["CloudTrailEvent"])
                if self._created(record, version):
                    return record["userIdentity"]["arn"], record["eventTime"]
            if not page.get("NextToken"):
                return None
            args["NextToken"] = page["NextToken"]

    def _created(self, record: dict, version: int) -> bool:
        request = record.get("requestParameters") or {}
        response = record.get("responseElements") or {}
        return (
            "errorCode" not in record
            and request.get("applicationId") == self._app
            and request.get("configurationProfileId") == self._profile
            and response.get("versionNumber") == version
        )
