"""End-to-end fixtures. Skipped unless ELEVATE_E2E=1 (set by `make e2e`): these tests
create and delete real resources in the test organization."""

import json
import os
import sys
import time
from datetime import UTC, datetime
from pathlib import Path

import pytest

if os.environ.get("ELEVATE_E2E") != "1":
    pytest.skip("end-to-end tests run only through `make e2e`", allow_module_level=True)

sys.path.insert(0, str(Path(__file__).parent))
import harness  # noqa: E402

from aws_elevate.adapters.grants_table import timeline_item, to_item  # noqa: E402
from aws_elevate.core.ids import new_grant_id  # noqa: E402
from aws_elevate.core.models import (  # noqa: E402
    Approval,
    Grant,
    PermissionSetRef,
    Principal,
    State,
)
from aws_elevate.core.times import iso  # noqa: E402

RULE_ID = "e2e-probe"


class Org:
    """Handles on the deployed e2e stack, used by the tests."""

    def __init__(self):
        self.human = harness.human_session()
        self.svc = harness.service_session(self.human)
        self.out = harness.stack_outputs(self.svc)
        self.instance = self.out["InstanceArn"]
        self.table = self.out["GrantsTableName"]
        self.engine = f"{harness.STACK}-engine"
        self.sfn = self.svc.client("stepfunctions")
        self.sso = self.svc.client("sso-admin")
        self.source_arn = harness.ensure_source_ps(self.svc, self.instance)
        self.user_id = self.svc.client("identitystore").get_user_id(
            IdentityStoreId=self.out["IdentityStoreId"],
            AlternateIdentifier={
                "UniqueAttribute": {
                    "AttributePath": "userName",
                    "AttributeValue": harness.env("TEST_USER"),
                }
            },
        )["UserId"]
        self.config_version = self._publish_rules()

    def _publish_rules(self) -> int:
        doc = {
            "schemaVersion": 1,
            "rules": [
                {
                    "id": RULE_ID,
                    "group": {"id": "g-e2e", "name": "e2e"},
                    "permissionSet": {"arn": self.source_arn, "name": harness.SOURCE_PS},
                    "accounts": [{"id": harness.env("TARGET_ACCOUNT"), "name": "target"}],
                    "duration": {"defaultSeconds": 300, "maxSeconds": 3600},
                    "approval": "auto",
                    "approverGroup": None,
                    "privileged": False,
                }
            ],
        }
        resp = self.svc.client("appconfig").create_hosted_configuration_version(
            ApplicationId=self.out["RulesApplicationId"],
            ConfigurationProfileId=self.out["RulesProfileId"],
            Content=json.dumps(doc).encode(),
            ContentType="application/json",
        )
        return resp["VersionNumber"]

    # ---------- grants ----------

    def new_grant(self, duration_seconds: int) -> Grant:
        grant_id = harness.GRANT_PREFIX + new_grant_id()[len(harness.GRANT_PREFIX) :]
        grant = Grant(
            id=grant_id,
            state=State.ACTIVATING,
            rule_id=RULE_ID,
            approval=Approval.AUTO,
            config_version=self.config_version,
            requester=Principal(user_id=self.user_id, user_name=harness.env("TEST_USER")),
            approver_group=None,
            source_ps=PermissionSetRef(arn=self.source_arn, name=harness.SOURCE_PS),
            account=harness.env("TARGET_ACCOUNT"),
            duration_seconds=duration_seconds,
            reason="aws-elevate e2e",
            privileged=False,
            created_at=iso(datetime.now(UTC)),
        )
        lock = {
            "pk": {"S": f"LOCK#{self.user_id}#{self.source_arn}#{grant.account}"},
            "sk": {"S": "LOCK"},
            "grantId": {"S": grant.id},
        }
        items = [
            {"Put": {"TableName": self.table, "Item": to_item(grant)}},
            {"Put": {"TableName": self.table, "Item": lock}},
            {"Put": {"TableName": self.table,
                     "Item": timeline_item(grant.id, grant.state, datetime.now(UTC), {})}},
        ]  # fmt: skip
        harness.invoke(self.svc, harness.SEEDER, {"op": "write", "TransactItems": items})
        return grant

    def start(self, grant: Grant) -> str:
        return self.sfn.start_execution(
            stateMachineArn=self.out["StateMachineArn"],
            name=grant.id,
            input=json.dumps({"grantId": grant.id, "approval": grant.approval.value}),
        )["executionArn"]

    def snapshot(self, grant_id: str) -> dict:
        return harness.invoke(self.svc, self.engine, {"action": "snapshot", "grantId": grant_id})[
            "grant"
        ]

    def raw(self, grant_id: str) -> dict:
        return harness.invoke(
            self.svc,
            harness.SEEDER,
            {
                "op": "get",
                "TableName": self.table,
                "Key": {"pk": {"S": f"GRANT#{grant_id}"}, "sk": {"S": "META"}},
            },
        )["Item"]

    def write(self, items: list[dict]) -> None:
        harness.invoke(self.svc, harness.SEEDER, {"op": "write", "TransactItems": items})

    def wait_state(self, grant_id: str, state: str, timeout_s: float = 180) -> dict:
        started = time.monotonic()
        while True:
            snap = self.snapshot(grant_id)
            if snap["state"] == state:
                return snap
            if time.monotonic() - started > timeout_s:
                raise AssertionError(f"grant {grant_id} still {snap['state']}, wanted {state}")
            time.sleep(3)

    def wait_execution(self, arn: str, status: str, timeout_s: float = 600) -> dict:
        started = time.monotonic()
        while True:
            desc = self.sfn.describe_execution(executionArn=arn)
            if desc["status"] == status:
                return desc
            if desc["status"] != "RUNNING" or time.monotonic() - started > timeout_s:
                raise AssertionError(f"execution is {desc['status']}, wanted {status}")
            time.sleep(5)

    def events_sent(self, arn: str) -> list[str]:
        """States of event tasks that succeeded, in order (putEvents accepted the entry)."""
        sent, current = [], ""
        pages = self.sfn.get_paginator("get_execution_history").paginate(executionArn=arn)
        for page in pages:
            for ev in page["events"]:
                if ev["type"] == "TaskStateEntered":
                    current = ev["stateEnteredEventDetails"]["name"]
                if ev["type"] == "TaskSucceeded" and current.startswith("Ev"):
                    output = json.loads(ev["taskSucceededEventDetails"]["output"])
                    assert output.get("FailedEntryCount", 0) == 0, (current, output)
                    sent.append(current)
        return sent

    def permission_set(self, name: str) -> str | None:
        for page in self.sso.get_paginator("list_permission_sets").paginate(
            InstanceArn=self.instance
        ):
            for arn in page["PermissionSets"]:
                ps = self.sso.describe_permission_set(
                    InstanceArn=self.instance, PermissionSetArn=arn
                )
                if ps["PermissionSet"]["Name"] == name:
                    return arn
        return None

    def tags(self, arn: str) -> dict:
        tags = self.sso.list_tags_for_resource(InstanceArn=self.instance, ResourceArn=arn)["Tags"]
        return {t["Key"]: t["Value"] for t in tags}


@pytest.fixture(scope="session")
def org():
    return Org()
