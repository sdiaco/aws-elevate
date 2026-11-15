"""Client and infrastructure versions (design §12.2). Each side declares the oldest
version of the other it works with; the pair is valid only if both conditions hold.
Cases are in contract/compat.json, shared with the Go client."""

import re
from enum import StrEnum

_VERSION = re.compile(r"^(\d+)\.(\d+)\.(\d+)$")


class Compat(StrEnum):
    OK = "ok"
    CLIENT_TOO_OLD = "client_too_old"
    INFRA_TOO_OLD = "infra_too_old"


def parse_version(text: str) -> tuple[int, int, int]:
    match = _VERSION.match(text)
    if not match:
        raise ValueError(f"version {text!r} is not major.minor.patch")
    return tuple(int(part) for part in match.groups())


def check(client: str, infra: str, min_client: str, min_infra: str) -> Compat:
    # Client too old wins when both are: upgrading the client is what the user can do.
    if parse_version(client) < parse_version(min_client):
        return Compat.CLIENT_TOO_OLD
    if parse_version(infra) < parse_version(min_infra):
        return Compat.INFRA_TOO_OLD
    return Compat.OK
