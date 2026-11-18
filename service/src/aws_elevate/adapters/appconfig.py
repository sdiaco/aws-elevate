"""AppConfig hosted configuration holding the rules document. Every saved
version is immutable and numbered; the latest valid one is in force."""

import json

from botocore.exceptions import ClientError


class NotJson(Exception):
    """A version whose content is not JSON. AppConfig stores any content."""


class AppConfig:
    def __init__(self, client, application_id: str, profile_id: str):
        self._appconfig = client
        self._ids = {"ApplicationId": application_id, "ConfigurationProfileId": profile_id}

    def list_versions(self) -> list[int]:
        """Every version number, newest first. The API does not document an order."""
        numbers, token = [], None
        while True:
            page = self._appconfig.list_hosted_configuration_versions(
                **self._ids, **({"NextToken": token} if token else {})
            )
            numbers += [item["VersionNumber"] for item in page["Items"]]
            token = page.get("NextToken")
            if not token:
                return sorted(numbers, reverse=True)

    def get(self, version: int) -> dict:
        resp = self._appconfig.get_hosted_configuration_version(**self._ids, VersionNumber=version)
        try:
            return json.loads(resp["Content"].read())
        except ValueError as err:
            raise NotJson(f"config version {version} is not JSON") from err

    def latest(self) -> tuple[int, dict] | None:
        versions = self.list_versions()
        return (versions[0], self.get(versions[0])) if versions else None

    def validate(self, version: int) -> tuple[bool, str | None]:
        """Check a version against the profile's schema. AppConfig applies the schema
        only on request, never when a version is created."""
        try:
            self._appconfig.validate_configuration(**self._ids, ConfigurationVersion=str(version))
        except ClientError as err:
            if err.response["Error"]["Code"] == "BadRequestException":
                return False, err.response["Error"].get("Message")
            raise
        return True, None
