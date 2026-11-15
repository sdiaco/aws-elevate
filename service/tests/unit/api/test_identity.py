"""Identity of a call comes only from the signature (design §8.1, spike C9)."""

import pytest

from aws_elevate.api.errors import ApiError
from aws_elevate.api.identity import IdentityResolver

ACCOUNT = "111111111111"
ADMIN_ROLE = "AWSReservedSSO_ElevateAdmin_0123456789abcdef"
REQ_ROLE = "AWSReservedSSO_ElevateRequester_fedcba9876543210"
SSO_PATH = "/aws-reserved/sso.amazonaws.com/eu-west-1/"
ROLES = {ADMIN_ROLE: ("AROAADMIN", SSO_PATH), REQ_ROLE: ("AROAREQ", SSO_PATH)}


class FakeRoles:
    def __init__(self, roles):
        self.roles = roles

    def role(self, name):
        return self.roles.get(name)


class FakeStore:
    def __init__(self):
        self.users = {"dev@example.com": "u-1"}
        self.groups = {"u-1": frozenset({"g-1", "g-audit"})}

    def user_id(self, name):
        return self.users.get(name)

    def group_ids(self, user_id):
        return self.groups[user_id]


def event(
    role=REQ_ROLE, user="dev@example.com", role_id="AROAREQ", account=ACCOUNT, caller_user=None
):
    return {
        "requestContext": {
            "accountId": ACCOUNT,
            "identity": {
                "userArn": f"arn:aws:sts::{account}:assumed-role/{role}/{user}",
                "caller": f"{role_id}:{caller_user or user}",
            },
        }
    }


def resolver(roles=ROLES, auditor="g-audit"):
    return IdentityResolver(
        FakeRoles(roles), FakeStore(), "ElevateRequester", "ElevateAdmin", auditor or None
    )


def test_requester():
    identity = resolver(auditor="").resolve(event())
    assert (identity.user_id, identity.user_name) == ("u-1", "dev@example.com")
    assert identity.group_ids == frozenset({"g-1", "g-audit"})
    assert not identity.is_admin and not identity.is_auditor


def test_admin():
    assert resolver().resolve(event(role=ADMIN_ROLE, role_id="AROAADMIN")).is_admin


def test_auditor_from_group():
    assert resolver().resolve(event()).is_auditor


def forbidden(evt, roles=ROLES):
    with pytest.raises(ApiError) as caught:
        resolver(roles).resolve(evt)
    assert (caught.value.status, caught.value.code) == (403, "forbidden")


def test_other_sso_role_refused():
    forbidden(event(role="AWSReservedSSO_JIT-abcde23456_0123456789abcdef"))


def test_role_with_a_similar_name_refused():
    forbidden(event(role="AWSReservedSSO_ElevateAdminX_0123456789abcdef"))


def test_role_id_that_does_not_match_refused():
    # A role recreated with the same name has a new RoleId.
    forbidden(event(role_id="AROAOTHER"))


def test_role_outside_the_sso_path_refused():
    forbidden(event(), roles={REQ_ROLE: ("AROAREQ", "/")})


def test_role_that_does_not_exist_refused():
    forbidden(event(), roles={})


def test_session_name_must_be_the_user():
    forbidden(event(caller_user="someone@example.com"))


def test_other_account_refused():
    forbidden(event(account="222222222222"))


def test_unknown_user_refused():
    forbidden(event(user="ghost@example.com"))


@pytest.mark.parametrize("identity", [{}, {"userArn": None, "caller": None}, {"userArn": "x"}])
def test_unsigned_or_malformed_refused(identity):
    forbidden({"requestContext": {"accountId": ACCOUNT, "identity": identity}})
