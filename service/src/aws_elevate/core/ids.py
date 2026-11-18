"""Grant identifiers and the names derived from them."""

import secrets

_ALPHABET = "abcdefghijklmnopqrstuvwxyz234567"  # lowercase base32
_LENGTH = 10


def new_grant_id() -> str:
    return "".join(secrets.choice(_ALPHABET) for _ in range(_LENGTH))


def ps_name(grant_id: str) -> str:
    return f"JIT-{grant_id}"


# Permission sets no rule may use as its source: the copies the service makes and deletes
# (JIT-<id>) and the service's own (ElevateRequester, ElevateAdmin, ElevateAuditor).
RESERVED_PREFIXES = ("JIT-", "Elevate")


def reserved_source(name: str) -> bool:
    return name.startswith(RESERVED_PREFIXES)
