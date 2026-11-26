"""Publishes the built-in e-mails to the approvals and alerts topics."""

from ..core.notify import Email


class Topics:
    def __init__(self, client, arns: dict[str, str]):
        self._sns = client
        self._arns = arns

    def publish(self, email: Email) -> None:
        self._sns.publish(
            TopicArn=self._arns[email.topic], Subject=email.subject, Message=email.body
        )
