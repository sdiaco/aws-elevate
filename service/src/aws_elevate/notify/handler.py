"""Entry point of the `notify` Lambda. EventBridge invokes it with an elevate event; it
sends the matching built-in e-mail, if any. A publish error is not caught: the failed
invocation makes Lambda retry the event."""

import os

from ..core.notify import compose

_topics = None


def send(event: dict, topics) -> bool:
    email = compose(event)
    if email is None:
        return False
    topics.publish(email)
    return True


def _build_topics():
    import boto3

    from ..adapters.sns import Topics

    return Topics(
        boto3.client("sns"),
        {
            "alerts": os.environ["ALERTS_TOPIC_ARN"],
            "approvals": os.environ["APPROVALS_TOPIC_ARN"],
        },
    )


def handler(event, context) -> None:
    global _topics
    if _topics is None:
        _topics = _build_topics()
    send(event, _topics)
