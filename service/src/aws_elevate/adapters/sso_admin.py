"""Writes to Identity Center: the ephemeral permission set and its assignment.

Only the engine imports this module (api must not: a test enforces it). Every operation
tolerates "already done", because Step Functions may run an engine step again.

Identity Center behaviors measured in the test organization on 2026-11-15:
- creating a permission set with an existing name, or attaching a policy twice, raises
  ConflictException, the same code used for concurrent modifications;
- creating an assignment that already exists succeeds;
- deleting an absent assignment, or provisioning where nothing is assigned, is accepted
  and then FAILS asynchronously with a reason starting "Received a 404 status error";
- deleting a permission set that is still assigned raises ConflictException.
"""

import time
from collections.abc import Callable

from botocore.exceptions import ClientError

from ..core.models import SourcePs
from ..core.policy import serialize
from .sso_read import SsoRead

RETRYABLE = {"ThrottlingException", "ConflictException", "InternalServerException"}
_NOT_FOUND_REASON = "Received a 404 status error"


class SsoOperationFailed(Exception):
    """Identity Center reported FAILED; the message is AWS's reason."""


class SsoTimeout(Exception):
    """An operation did not finish within the time budget."""


def _code(err: ClientError) -> str:
    return err.response.get("Error", {}).get("Code", "")


class SsoAdmin:
    def __init__(
        self,
        client,
        instance_arn: str,
        reader: SsoRead,
        sleep: Callable[[float], None] = time.sleep,
        clock: Callable[[], float] = time.monotonic,
        budget_s: float = 120,
    ):
        self._sso = client
        self._instance = instance_arn
        self._reader = reader
        self._sleep = sleep
        self._clock = clock
        self._budget = budget_s

    # ---------- helpers ----------

    def _retry(self, call: Callable[[], dict], retry_on=RETRYABLE) -> dict:
        start, delay = self._clock(), 1.0
        while True:
            try:
                return call()
            except ClientError as err:
                if _code(err) not in retry_on or self._clock() - start + delay > self._budget:
                    raise
                self._sleep(delay)
                delay = min(delay * 2, 10)

    def _wait(self, fetch: Callable[[], dict], what: str) -> None:
        """Poll an asynchronous request; raise SsoOperationFailed with AWS's reason."""
        start = self._clock()
        while True:
            status = fetch()
            if status["Status"] == "SUCCEEDED":
                return
            if status["Status"] == "FAILED":
                raise SsoOperationFailed(f"{what}: {status.get('FailureReason', 'no reason')}")
            if self._clock() - start > self._budget:
                raise SsoTimeout(f"{what} still in progress after {self._budget:.0f}s")
            self._sleep(1)

    def _kw(self, ps_arn: str) -> dict:
        return {"InstanceArn": self._instance, "PermissionSetArn": ps_arn}

    def _target(self, ps_arn: str, account: str) -> dict:
        return {**self._kw(ps_arn), "TargetId": account, "TargetType": "AWS_ACCOUNT"}

    # ---------- operations ----------

    def create_ephemeral(
        self, name: str, description: str, session_duration: str, tags: dict[str, str]
    ) -> str:
        """Create the ephemeral permission set, or find it if an earlier attempt did.
        An empty description is omitted (the API requires at least one character)."""
        params = {
            "InstanceArn": self._instance,
            "Name": name,
            "SessionDuration": session_duration,
            "Tags": [{"Key": k, "Value": v} for k, v in tags.items()],
        }
        if description:
            params["Description"] = description
        start = self._clock()
        while True:
            try:
                resp = self._retry(
                    lambda: self._sso.create_permission_set(**params),
                    retry_on=RETRYABLE - {"ConflictException"},
                )
                return resp["PermissionSet"]["PermissionSetArn"]
            except ClientError as err:
                if _code(err) != "ConflictException":
                    raise
                # Either the name exists (an earlier attempt created it) or a concurrent
                # change is in progress: look before trying again.
                existing = self._reader.find_by_name(name)
                if existing:
                    return existing
                if self._clock() - start > self._budget:
                    raise
                self._sleep(2)

    def configure(self, arn: str, inline: dict, source: SourcePs) -> None:
        """Copy into the ephemeral permission set the source's managed and customer
        managed policies and boundary, plus `inline`. Safe to repeat."""
        kw = self._kw(arn)
        self._retry(
            lambda: self._sso.put_inline_policy_to_permission_set(
                InlinePolicy=serialize(inline), **kw
            )
        )
        attached = self._reader.managed_arns(arn)
        for policy in source.managed:
            if policy not in attached:
                self._retry(
                    lambda p=policy: self._sso.attach_managed_policy_to_permission_set(
                        ManagedPolicyArn=p, **kw
                    )
                )
        attached_refs = self._reader.customer_managed_refs(arn)
        for ref in source.customer_managed:
            if ref not in attached_refs:
                self._retry(
                    lambda r=ref: (
                        self._sso.attach_customer_managed_policy_reference_to_permission_set(  # noqa: E501
                            CustomerManagedPolicyReference=r, **kw
                        )
                    )
                )
        if source.boundary:
            self._retry(
                lambda: self._sso.put_permissions_boundary_to_permission_set(
                    PermissionsBoundary=source.boundary, **kw
                )
            )

    def assign(self, ps_arn: str, account: str, user_id: str) -> None:
        req = self._retry(
            lambda: self._sso.create_account_assignment(
                **self._target(ps_arn, account), PrincipalType="USER", PrincipalId=user_id
            )
        )["AccountAssignmentCreationStatus"]
        self._wait(
            lambda: self._sso.describe_account_assignment_creation_status(
                InstanceArn=self._instance,
                AccountAssignmentCreationRequestId=req["RequestId"],
            )["AccountAssignmentCreationStatus"],
            "assignment",
        )

    def rewrite_inline(self, ps_arn: str, account: str, inline: dict) -> None:
        """Replace the inline policy and reprovision, so open sessions see it at once
        (measured: without reprovisioning the rewrite does not reach them)."""
        try:
            self._retry(
                lambda: self._sso.put_inline_policy_to_permission_set(
                    InlinePolicy=serialize(inline), **self._kw(ps_arn)
                )
            )
        except ClientError as err:
            if _code(err) == "ResourceNotFoundException":
                return  # permission set already deleted: nothing left to deny
            raise
        req = self._retry(
            lambda: self._sso.provision_permission_set(**self._target(ps_arn, account))
        )["PermissionSetProvisioningStatus"]
        try:
            self._wait(
                lambda: self._sso.describe_permission_set_provisioning_status(
                    InstanceArn=self._instance,
                    ProvisionPermissionSetRequestId=req["RequestId"],
                )["PermissionSetProvisioningStatus"],
                "reprovisioning",
            )
        except SsoOperationFailed as err:
            if _NOT_FOUND_REASON not in str(err):
                raise
            # Not provisioned in the account any more: no role, so no open session.

    def unassign(self, ps_arn: str, account: str, user_id: str) -> None:
        try:
            req = self._retry(
                lambda: self._sso.delete_account_assignment(
                    **self._target(ps_arn, account), PrincipalType="USER", PrincipalId=user_id
                )
            )["AccountAssignmentDeletionStatus"]
        except ClientError as err:
            if _code(err) == "ResourceNotFoundException":
                return
            raise
        try:
            self._wait(
                lambda: self._sso.describe_account_assignment_deletion_status(
                    InstanceArn=self._instance,
                    AccountAssignmentDeletionRequestId=req["RequestId"],
                )["AccountAssignmentDeletionStatus"],
                "unassignment",
            )
        except SsoOperationFailed as err:
            if _NOT_FOUND_REASON not in str(err):
                raise

    def delete(self, ps_arn: str) -> None:
        try:
            self._retry(lambda: self._sso.delete_permission_set(**self._kw(ps_arn)))
        except ClientError as err:
            if _code(err) != "ResourceNotFoundException":
                raise
