"""Rules version routes of api. The list and the content pass through
api so the auditor sees them without any AWS permission of their own."""

from ..core.models import ConfigVersion, Identity
from .config_cache import ConfigUnavailable
from .errors import ApiError, forbidden


def _version(record: ConfigVersion) -> dict:
    return {
        "number": record.number,
        "valid": record.valid,
        "errors": list(record.errors),
        "author": record.author,
        "createdAt": record.created_at,
        "verified": record.verified,
    }


def _allow(identity: Identity) -> None:
    if not (identity.is_admin or identity.is_auditor):
        raise forbidden("only administrators and auditors can read rules versions")


class ConfigRoutes:
    def __init__(self, config):
        self._config = config

    def list(self, identity: Identity) -> dict:
        _allow(identity)
        try:
            current, _ = self._config.current()
        except ConfigUnavailable:
            current = None
        return {"current": current, "versions": [_version(v) for v in self._config.versions()]}

    def get(self, identity: Identity, number: str) -> dict:
        _allow(identity)
        found = self._config.version(int(number))
        if found is None:
            raise ApiError(404, "not_found", f"no rules version {number}")
        record, doc = found
        return {"version": _version(record), "doc": doc}
