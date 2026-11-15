"""One timestamp format everywhere: UTC, second precision, `Z` suffix. It sorts as text
(table keys) and matches what aws:CurrentTime is compared with."""

from datetime import UTC, datetime

_FORMAT = "%Y-%m-%dT%H:%M:%SZ"


def iso(moment: datetime) -> str:
    if moment.tzinfo is None:
        raise ValueError("timestamps must be timezone-aware")
    return moment.astimezone(UTC).strftime(_FORMAT)


def parse_iso(text: str) -> datetime:
    return datetime.strptime(text, _FORMAT).replace(tzinfo=UTC)
