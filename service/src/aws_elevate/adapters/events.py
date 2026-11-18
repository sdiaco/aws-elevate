"""Events api publishes on the elevate bus. Grant events come from the state machine;
api emits only elevate.config.rejected."""

import json


class EventPublishFailed(Exception):
    pass


class Events:
    def __init__(self, client, bus_name: str):
        self._events = client
        self._bus = bus_name

    def put_config_rejected(self, version: int, author: str | None, errors: list[str]) -> None:
        detail = {
            "schemaVersion": 1,
            "eventId": f"config:{version}:rejected",
            "configVersion": version,
            "author": author,
            "errors": errors,
        }
        resp = self._events.put_events(
            Entries=[
                {
                    "Source": "elevate",
                    "DetailType": "elevate.config.rejected",
                    "EventBusName": self._bus,
                    "Detail": json.dumps(detail),
                }
            ]
        )
        if resp.get("FailedEntryCount"):
            failure = resp["Entries"][0]
            raise EventPublishFailed(
                f"elevate.config.rejected for version {version}: "
                f"{failure.get('ErrorCode')} {failure.get('ErrorMessage')}"
            )
