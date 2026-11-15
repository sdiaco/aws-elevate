"""AppConfig hosted configuration holding the rules document (design §7.1). Every saved
version is immutable and numbered; the latest valid one is in force."""

import json


class AppConfig:
    def __init__(self, client, application_id: str, profile_id: str):
        self._appconfig = client
        self._app = application_id
        self._profile = profile_id

    def get(self, version: int) -> dict:
        resp = self._appconfig.get_hosted_configuration_version(
            ApplicationId=self._app, ConfigurationProfileId=self._profile, VersionNumber=version
        )
        return json.loads(resp["Content"].read())
