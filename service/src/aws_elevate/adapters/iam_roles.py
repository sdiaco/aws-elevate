"""The real IAM roles behind the two service permission sets. api compares the RoleId of
a signed call with these, so a role that only imitates the name is refused."""

from botocore.exceptions import ClientError


class IamRoles:
    def __init__(self, client):
        self._iam = client
        self._cache: dict[str, tuple[str, str] | None] = {}

    def role(self, name: str) -> tuple[str, str] | None:
        """(RoleId, Path) of a role, read once per container; None if it does not exist."""
        if name not in self._cache:
            try:
                role = self._iam.get_role(RoleName=name)["Role"]
                self._cache[name] = (role["RoleId"], role["Path"])
            except ClientError as err:
                if err.response["Error"]["Code"] != "NoSuchEntity":
                    raise
                self._cache[name] = None
        return self._cache[name]
