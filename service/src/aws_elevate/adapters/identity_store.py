"""Identity Center users and groups. Group membership is read on every call, so a
change in Identity Center applies at once."""

from botocore.exceptions import ClientError


class IdentityStore:
    def __init__(self, client, store_id: str):
        self._ids = client
        self._store = store_id

    def user_id(self, user_name: str) -> str | None:
        try:
            resp = self._ids.get_user_id(
                IdentityStoreId=self._store,
                AlternateIdentifier={
                    "UniqueAttribute": {"AttributePath": "userName", "AttributeValue": user_name}
                },
            )
        except ClientError as err:
            if err.response["Error"]["Code"] == "ResourceNotFoundException":
                return None
            raise
        return resp["UserId"]

    def group_ids(self, user_id: str) -> frozenset[str]:
        args = {"IdentityStoreId": self._store, "MemberId": {"UserId": user_id}}
        groups: set[str] = set()
        while True:
            page = self._ids.list_group_memberships_for_member(**args)
            groups |= {m["GroupId"] for m in page["GroupMemberships"]}
            if not page.get("NextToken"):
                return frozenset(groups)
            args["NextToken"] = page["NextToken"]
