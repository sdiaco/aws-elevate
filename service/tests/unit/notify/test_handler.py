import json
from pathlib import Path

from aws_elevate.core.notify import Email
from aws_elevate.notify import handler as notify
from aws_elevate.notify.handler import send

EVENTS = Path(__file__).parents[4] / "contract" / "events"


class FakeTopics:
    def __init__(self):
        self.published = []

    def publish(self, email):
        self.published.append(email)


def load(name):
    example = json.loads((EVENTS / name).read_text())
    return {
        "source": "elevate",
        "detail-type": example["detailType"],
        "time": "2026-11-15T11:01:05Z",
        "detail": example["detail"],
    }


def test_send_publishes_the_composed_email():
    topics = FakeTopics()
    assert send(load("elevate.grant.error.json"), topics) is True
    assert len(topics.published) == 1
    assert isinstance(topics.published[0], Email)
    assert topics.published[0].topic == "alerts"


def test_send_skips_events_without_email():
    topics = FakeTopics()
    assert send(load("elevate.config.rejected.json"), topics) is False
    assert topics.published == []


def test_handler_builds_the_topics_once_and_sends(monkeypatch):
    topics = FakeTopics()
    builds = []
    monkeypatch.setattr(notify, "_topics", None)
    monkeypatch.setattr(notify, "_build_topics", lambda: builds.append(1) or topics)
    event = load("elevate.grant.error.json")
    notify.handler(event, None)
    notify.handler(event, None)
    assert len(builds) == 1
    assert len(topics.published) == 2
