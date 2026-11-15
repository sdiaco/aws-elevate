"""Rules in force for api (design §7.1, §7.3).

The latest valid version wins. Every version is evaluated once (schema check by
AppConfig plus the full parser) and recorded in the table; an invalid one is skipped
and reported with one elevate.config.rejected event, whichever container sees it
first. The author comes from CloudTrail, which can lag by minutes, so it is asked
again on later reads until it answers."""

import dataclasses
import logging
import time
from collections.abc import Callable

from botocore.exceptions import ClientError

from ..adapters.appconfig import NotJson
from ..adapters.events import EventPublishFailed
from ..core.models import ConfigVersion, Rule
from ..core.rules import InvalidRules, parse

REFRESH_SECONDS = 30  # design §6.6, §7.1

log = logging.getLogger(__name__)


class ConfigUnavailable(Exception):
    """No valid rules version exists: api answers 503 config_unavailable."""


class ConfigCache:
    def __init__(
        self, appconfig, table, trail, events, clock: Callable[[], float] = time.monotonic
    ):
        self._appconfig = appconfig
        self._table = table
        self._trail = trail
        self._events = events
        self._clock = clock
        self._version: ConfigVersion | None = None
        self._rules: list[Rule] = []
        self._checked_at: float | None = None

    def current(self) -> tuple[int, list[Rule]]:
        now = self._clock()
        if self._version is None or now - self._checked_at >= REFRESH_SECONDS:
            self._refresh()
            self._checked_at = now
        return self._version.number, self._rules

    def versions(self) -> list[ConfigVersion]:
        """Every version, newest first, as recorded. A version never seen is evaluated
        and recorded now. No CloudTrail lookup here: LookupEvents allows 2 calls per
        second, and the refresh fills the author of every new version anyway."""
        return [self._record(n) for n in self._appconfig.list_versions()]

    def version(self, number: int) -> tuple[ConfigVersion, dict | None] | None:
        """Record and content of one version; content None if it is not JSON."""
        if number not in self._appconfig.list_versions():
            return None
        record = self._record(number)
        try:
            return record, self._appconfig.get(number)
        except NotJson:
            return record, None

    def _refresh(self) -> None:
        for number in self._appconfig.list_versions():
            if self._version and number == self._version.number:
                self._version = self._with_author(self._version)
                return
            record = self._with_author(self._record(number))
            if not record.valid:
                self._report(record)
                continue
            rules = self._rules_of(number)
            if rules is not None:
                self._version, self._rules = record, rules
                return
        raise ConfigUnavailable("no valid rules version: publish one with aws elevate admin")

    def _record(self, number: int) -> ConfigVersion:
        record = self._table.get_config(number)
        if record is None:
            record = self._evaluate(number)
            if not self._table.put_config(record):
                record = self._table.get_config(number)
        return record

    def _evaluate(self, number: int) -> ConfigVersion:
        errors = []
        schema_ok, message = self._appconfig.validate(number)
        if not schema_ok:
            errors.append(f"schema: {message}")
        try:
            parse(self._appconfig.get(number))
        except NotJson as err:
            errors.append(str(err))
        except InvalidRules as err:
            errors += err.errors
        return ConfigVersion(number=number, valid=not errors, errors=tuple(errors))

    def _rules_of(self, number: int) -> list[Rule] | None:
        try:
            return parse(self._appconfig.get(number))
        except (NotJson, InvalidRules) as err:
            # Recorded valid but no longer parses (a newer service is stricter):
            # fall back to an older version rather than stop serving.
            log.error("config version %s recorded valid but does not parse: %s", number, err)
            return None

    def _with_author(self, record: ConfigVersion) -> ConfigVersion:
        if record.verified:
            return record
        found = self._trail.version_author(record.number)
        if found is None:
            return record
        author, created_at = found
        self._table.set_config_author(record.number, author, created_at)
        return dataclasses.replace(record, author=author, created_at=created_at, verified=True)

    def _report(self, record: ConfigVersion) -> None:
        if record.rejected_event_sent or not self._table.claim_rejected_event(record.number):
            return
        try:
            self._events.put_config_rejected(record.number, record.author, list(record.errors))
        except (EventPublishFailed, ClientError) as err:
            self._table.release_rejected_event(record.number)
            log.error(
                "elevate.config.rejected for version %s not sent (%s); retrying on the next read",
                record.number,
                err,
            )
