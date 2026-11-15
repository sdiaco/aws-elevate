"""Every event elevate publishes has an example in contract/events/ with the same detail
keys: the grant events built by the state machine from the engine snapshot, and the
config event emitted by api (design §9.1)."""

import dataclasses
import json
from pathlib import Path

import pytest

from aws_elevate.adapters.events import Events
from aws_elevate.core.events import snapshot
from aws_elevate.core.models import CloseReason, Principal, State
from aws_elevate.statemachine import definition

EVENTS = Path(__file__).resolve().parents[3] / "contract" / "events"
STATES = definition("arn:aws:lambda:eu-west-1:111111111111:function:engine", "elevate-events")[
    "States"
]
# The state machine names these from the close reason of a grant that never started.
NOT_APPROVED = ("rejected", "timed_out", "cancelled", "invalidated")


def example(detail_type: str) -> dict:
    return json.loads((EVENTS / f"{detail_type}.json").read_text())


def event_entries() -> dict[str, dict]:
    return {
        name: state["Parameters"]["Entries"][0]
        for name, state in STATES.items()
        if state.get("Resource", "").endswith("events:putEvents")
    }


def detail_types(entry: dict) -> list[str]:
    if "DetailType" in entry:
        return [entry["DetailType"]]
    return [f"elevate.request.{reason}" for reason in NOT_APPROVED]


def test_every_published_event_has_an_example():
    published = {t for e in event_entries().values() for t in detail_types(e)}
    published.add("elevate.config.rejected")
    assert sorted(p.stem for p in EVENTS.glob("*.json")) == sorted(published)


@pytest.mark.parametrize("name", sorted(p.stem for p in EVENTS.glob("*.json")))
def test_example_is_well_formed(name):
    ex = example(name)
    assert set(ex) == {"source", "detailType", "detail"}
    assert ex["source"] == "elevate"
    assert ex["detailType"] == name
    assert ex["detail"]["schemaVersion"] == 1


@pytest.mark.parametrize("state_name", sorted(event_entries()))
def test_state_machine_event_has_the_example_keys(state_name):
    entry = event_entries()[state_name]
    keys = {k.removesuffix(".$") for k in entry["Detail"]}
    for detail_type in detail_types(entry):
        assert keys == set(example(detail_type)["detail"]), detail_type


@pytest.mark.parametrize("detail_type", sorted(p.stem for p in EVENTS.glob("elevate.[rg]*.json")))
def test_grant_event_values_have_the_snapshot_types(fx, detail_type):
    # A grant with every field filled, so each type is checked.
    grant = dataclasses.replace(
        fx.make_grant(),
        state=State.CLOSED,
        close_reason=CloseReason.REVOKED,
        approver=Principal("u-2", "lead@example.com"),
        activated_at="2026-11-15T10:01:00Z",
        expires_at="2026-11-15T11:01:00Z",
        closed_at="2026-11-15T10:30:00Z",
        revoked_by=Principal("u-3", "admin@example.com"),
        revoke_reason="incident closed",
        error="none",
    )
    snap = snapshot(grant)
    detail = example(detail_type)["detail"]
    fx.assert_same_shape(
        {k: v for k, v in detail.items() if k in snap}, {k: snap[k] for k in detail if k in snap}
    )


class PutEventsClient:
    def __init__(self):
        self.entries = []

    def put_events(self, Entries):
        self.entries += Entries
        return {"FailedEntryCount": 0, "Entries": [{"EventId": "1"}]}


def test_config_rejected_event_matches_the_example(fx):
    client = PutEventsClient()
    Events(client, "elevate-events").put_config_rejected(4, "admin@example.com", ["schema: x"])
    (entry,) = client.entries
    ex = example("elevate.config.rejected")
    assert entry["DetailType"] == ex["detailType"]
    assert entry["Source"] == ex["source"]
    fx.assert_same_shape(ex["detail"], json.loads(entry["Detail"]))
