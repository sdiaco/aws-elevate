"""Errors the state machine matches by name: `Retryable` has a Retry policy, everything
else (including ActivationFailed) goes to the Catch that revokes."""


class Retryable(Exception):
    """Temporary failure (throttling, timeout): Step Functions runs the step again."""


class ActivationFailed(Exception):
    """Activation cannot succeed; the reason is saved in the grant record."""
