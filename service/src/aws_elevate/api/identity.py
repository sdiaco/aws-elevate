"""Who is calling, from the request signature only (design §8.1, spike C9).

API Gateway already admits only the service roles; api checks again, and also
compares the RoleId of the signature with the real role, so a role that imitates the
name is refused. Approver and auditor are group memberships, read on every call."""

import logging
import re

from ..core.models import Identity
from .errors import forbidden

SSO_ROLE_PATH = "/aws-reserved/sso.amazonaws.com/"
_ASSUMED_ROLE = re.compile(r"^arn:aws[a-z-]*:sts::(\d{12}):assumed-role/([^/]+)/([^/]+)$")

log = logging.getLogger(__name__)


class IdentityResolver:
    def __init__(
        self,
        roles,
        store,
        requester_ps: str,
        admin_ps: str,
        auditor_ps: str,
        auditor_group: str | None,
    ):
        self._roles = roles
        self._store = store
        # ElevateAuditor is ElevateRequester under a clearer name: auditing comes from the
        # auditor group, never from the role (design §3.3).
        self._prefixes = {
            f"AWSReservedSSO_{requester_ps}_": False,
            f"AWSReservedSSO_{admin_ps}_": True,
            f"AWSReservedSSO_{auditor_ps}_": False,
        }
        self._auditor_group = auditor_group

    def resolve(self, event: dict) -> Identity:
        context = event.get("requestContext") or {}
        signed = context.get("identity") or {}
        match = _ASSUMED_ROLE.match(signed.get("userArn") or "")
        if not match:
            raise self._refuse("call not signed by an assumed role", signed)
        account, role_name, user_name = match.groups()
        if account != context.get("accountId"):
            raise self._refuse("role of another account", signed)
        is_admin = self._service_role(role_name)
        if is_admin is None:
            raise self._refuse("not one of the service roles", signed)
        role_id, _, session = (signed.get("caller") or "").partition(":")
        if session != user_name:
            raise self._refuse("session name is not the user", signed)
        real = self._roles.role(role_name)
        if real is None or not real[1].startswith(SSO_ROLE_PATH) or real[0] != role_id:
            raise self._refuse("RoleId or path does not match the real role", signed)
        user_id = self._store.user_id(user_name)
        if user_id is None:
            raise self._refuse("user not found in Identity Center", signed)
        groups = self._store.group_ids(user_id)
        return Identity(
            user_id=user_id,
            user_name=user_name,
            group_ids=groups,
            is_admin=is_admin,
            is_auditor=self._auditor_group is not None and self._auditor_group in groups,
        )

    def _service_role(self, role_name: str) -> bool | None:
        """True for ElevateAdmin, False for ElevateRequester or ElevateAuditor, else None."""
        for prefix, is_admin in self._prefixes.items():
            suffix = role_name.removeprefix(prefix)
            if suffix != role_name and re.fullmatch(r"[0-9a-f]+", suffix):
                return is_admin
        return None

    @staticmethod
    def _refuse(why: str, signed: dict):
        # The caller gets a generic 403; the reason goes to the log for the operator.
        log.warning("refused call from %s: %s", signed.get("userArn"), why)
        return forbidden()
