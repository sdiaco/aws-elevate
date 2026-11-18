"""Step Functions calls made by api: one execution per grant, named after the grant id,
the end of a wait, and the redrive of a failed revocation."""

import json

from botocore.exceptions import ClientError


def _code(err: ClientError) -> str:
    return err.response.get("Error", {}).get("Code", "")


class StepFunctions:
    def __init__(self, client, state_machine_arn: str):
        self._sfn = client
        self._arn = state_machine_arn

    def execution_arn(self, grant_id: str) -> str:
        prefix, _, name = self._arn.rpartition(":stateMachine:")
        return f"{prefix}:execution:{name}:{grant_id}"

    def start(self, grant_id: str, approval: str) -> None:
        """Start the execution of a grant. Starting it twice is harmless: the name is
        the grant id, so a retry never runs the same grant twice."""
        try:
            self._sfn.start_execution(
                stateMachineArn=self._arn,
                name=grant_id,
                input=json.dumps({"grantId": grant_id, "approval": approval}),
            )
        except ClientError as err:
            if _code(err) != "ExecutionAlreadyExists":
                raise

    def send_success(self, token: str, output: dict) -> bool:
        """End a wait. False if the wait is already over: the record has been written,
        and the timeout branch reads it, so nothing is lost ("the record wins")."""
        try:
            self._sfn.send_task_success(taskToken=token, output=json.dumps(output))
        except ClientError as err:
            if _code(err) in ("TaskTimedOut", "TaskDoesNotExist"):
                return False
            raise
        return True

    def redrive(self, grant_id: str) -> bool:
        """Resume a failed execution from the failed task. False if it cannot be."""
        try:
            self._sfn.redrive_execution(executionArn=self.execution_arn(grant_id))
        except ClientError as err:
            if _code(err) == "ExecutionNotRedrivable":
                return False
            raise
        return True
