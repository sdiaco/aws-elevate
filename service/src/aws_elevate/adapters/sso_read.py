"""Read-only access to Identity Center permission sets. Used by api and engine; contains
no write call, so api can depend on it without crossing the security boundary."""

import json

from botocore.exceptions import ClientError

from ..core.models import SourcePs


class PsNotFound(Exception):
    """The permission set does not exist (deleted in AWS)."""


def _code(err: ClientError) -> str:
    return err.response.get("Error", {}).get("Code", "")


class SsoRead:
    def __init__(self, client, instance_arn: str):
        self._sso = client
        self._instance = instance_arn

    def _paged(self, operation: str, key: str, **kwargs) -> list:
        pages = self._sso.get_paginator(operation).paginate(InstanceArn=self._instance, **kwargs)
        return [entry for page in pages for entry in page[key]]

    def read_source(self, ps_arn: str) -> SourcePs:
        """Everything the ephemeral copy needs."""
        kw = {"InstanceArn": self._instance, "PermissionSetArn": ps_arn}
        try:
            described = self._sso.describe_permission_set(**kw)["PermissionSet"]
        except ClientError as err:
            if _code(err) == "ResourceNotFoundException":
                raise PsNotFound(ps_arn) from err
            raise
        inline = self._sso.get_inline_policy_for_permission_set(**kw).get("InlinePolicy")
        managed = self._paged(
            "list_managed_policies_in_permission_set",
            "AttachedManagedPolicies",
            PermissionSetArn=ps_arn,
        )
        customer = self._paged(
            "list_customer_managed_policy_references_in_permission_set",
            "CustomerManagedPolicyReferences",
            PermissionSetArn=ps_arn,
        )
        try:
            boundary = self._sso.get_permissions_boundary_for_permission_set(**kw)[
                "PermissionsBoundary"
            ]
        except ClientError as err:
            # No boundary is reported as ResourceNotFoundException.
            if _code(err) != "ResourceNotFoundException":
                raise
            boundary = None
        return SourcePs(
            arn=ps_arn,
            name=described["Name"],
            managed=tuple(m["Arn"] for m in managed),
            customer_managed=tuple(customer),
            boundary=boundary,
            inline=json.loads(inline) if inline else None,
        )

    def managed_arns(self, ps_arn: str) -> set[str]:
        entries = self._paged(
            "list_managed_policies_in_permission_set",
            "AttachedManagedPolicies",
            PermissionSetArn=ps_arn,
        )
        return {m["Arn"] for m in entries}

    def customer_managed_refs(self, ps_arn: str) -> list[dict]:
        return self._paged(
            "list_customer_managed_policy_references_in_permission_set",
            "CustomerManagedPolicyReferences",
            PermissionSetArn=ps_arn,
        )

    def find_by_name(self, name: str) -> str | None:
        """ARN of the permission set with this name. Lists the whole instance: used only
        to recover after a retried create, never on the normal path."""
        for arn in self._paged("list_permission_sets", "PermissionSets"):
            described = self._sso.describe_permission_set(
                InstanceArn=self._instance, PermissionSetArn=arn
            )["PermissionSet"]
            if described["Name"] == name:
                return arn
        return None
