from datetime import UTC, datetime, timedelta, timezone

import pytest

from aws_elevate.core.times import iso, parse_iso


def test_iso_is_utc_with_second_precision():
    moment = datetime(2026, 11, 15, 11, 30, 5, 999, tzinfo=timezone(timedelta(hours=1)))
    assert iso(moment) == "2026-11-15T10:30:05Z"


def test_parse_iso_round_trip():
    assert parse_iso("2026-11-15T10:30:05Z") == datetime(2026, 11, 15, 10, 30, 5, tzinfo=UTC)


def test_iso_rejects_naive_datetime():
    with pytest.raises(ValueError):
        iso(datetime(2026, 11, 15))
