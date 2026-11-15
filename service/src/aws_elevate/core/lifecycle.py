"""Allowed grant state transitions (design §3.2, §6). Every write to the grants table
is conditional on the source state; this table says which targets are legal."""

from .models import State

TRANSITIONS: dict[State, frozenset[State]] = {
    State.PENDING: frozenset({State.ACTIVATING, State.CLOSED}),
    State.ACTIVATING: frozenset({State.ACTIVE, State.CLOSED}),
    State.ACTIVE: frozenset({State.REVOKING}),
    State.REVOKING: frozenset({State.CLOSED, State.REVOKE_FAILED}),
    # Redrive after a failed revocation.
    State.REVOKE_FAILED: frozenset({State.REVOKING}),
    State.CLOSED: frozenset(),
}


def can_transition(src: State, dst: State) -> bool:
    return dst in TRANSITIONS[src]
