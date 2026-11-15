"""Grant identifiers and the names derived from them (design §5.1)."""

import secrets

_ALPHABET = "abcdefghijklmnopqrstuvwxyz234567"  # lowercase base32
_LENGTH = 10


def new_grant_id() -> str:
    return "".join(secrets.choice(_ALPHABET) for _ in range(_LENGTH))


def ps_name(grant_id: str) -> str:
    return f"JIT-{grant_id}"
