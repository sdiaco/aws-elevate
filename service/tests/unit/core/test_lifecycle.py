import itertools

from aws_elevate.core.lifecycle import TRANSITIONS, can_transition
from aws_elevate.core.models import State

ALLOWED = {
    (State.PENDING, State.ACTIVATING),
    (State.PENDING, State.CLOSED),
    (State.ACTIVATING, State.ACTIVE),
    (State.ACTIVATING, State.CLOSED),
    (State.ACTIVE, State.REVOKING),
    (State.REVOKING, State.CLOSED),
    (State.REVOKING, State.REVOKE_FAILED),
    (State.REVOKE_FAILED, State.REVOKING),
}


def test_every_pair_of_states_matches_the_table():
    for src, dst in itertools.product(State, repeat=2):
        assert can_transition(src, dst) == ((src, dst) in ALLOWED), (src, dst)


def test_closed_is_final():
    assert TRANSITIONS[State.CLOSED] == frozenset()


def test_every_state_has_an_entry():
    assert set(TRANSITIONS) == set(State)
