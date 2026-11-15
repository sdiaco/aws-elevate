"""Grant lifecycle as an Amazon States Language definition (design §4.2, §6, §9.1).

One execution per grant, named after the grant id; input {"grantId", "approval"}.
The engine payload is always {action, grantId[, taskToken]}. After every wait the grant
record, not the task result, decides what happens next ("the record wins").

Data kept in the execution: $.snap (snapshot after start and after the approval wait),
$.act (activation result), $.rev (revocation result). Every engine action returns a
snapshot of the record, from which the events take their fields."""

import json

_LAMBDA = "arn:aws:states:::lambda:invoke"
_LAMBDA_WAIT = "arn:aws:states:::lambda:invoke.waitForTaskToken"
_PUT_EVENTS = "arn:aws:states:::events:putEvents"
APPROVAL_TIMEOUT_SECONDS = 1800  # design §6.2

_ENGINE_RETRY = [
    {
        "ErrorEquals": [
            "Retryable",
            "Lambda.ServiceException",
            "Lambda.AWSLambdaException",
            "Lambda.SdkClientException",
            "Lambda.TooManyRequestsException",
        ],
        "IntervalSeconds": 2,
        "MaxAttempts": 5,
        "BackoffRate": 2,
    }
]
_EVENT_RETRY = [
    {"ErrorEquals": ["States.ALL"], "IntervalSeconds": 1, "MaxAttempts": 3, "BackoffRate": 2}
]
_EVENT_FIELDS = (
    "grantId", "requester", "approver", "approverGroup", "rule", "sourcePs", "account",
    "approval", "privileged", "startsAt", "expiresAt", "configVersion", "reason",
    "closeReason", "revokedBy", "revokeReason", "error",
)  # fmt: skip


def _engine(engine_arn: str, action: str, nxt: str, *, wait=False, **extra) -> dict:
    payload = {"action": action, "grantId.$": "$.grantId"}
    if wait:
        payload["taskToken.$"] = "$$.Task.Token"
    state = {
        "Type": "Task",
        "Resource": _LAMBDA_WAIT if wait else _LAMBDA,
        "Parameters": {"FunctionName": engine_arn, "Payload": payload},
        "Retry": _ENGINE_RETRY,
        "Next": nxt,
    }
    state.update(extra)
    return state


def _event(bus: str, snap: str, transition: str | None, detail_type: str | None, nxt: str,
           **detail_extra) -> dict:  # fmt: skip
    """putEvents with the event fields taken from the snapshot at `snap`. A lost event
    is caught and skipped: it must never stop a revocation."""
    detail = {"schemaVersion": 1}
    if transition:
        detail["eventId.$"] = f"States.Format('{{}}:{transition}', $.grantId)"
    else:
        detail["eventId.$"] = "States.Format('{}:{}', $.grantId, $.snap.grant.closeReason)"
    detail.update({f"{field}.$": f"{snap}.{field}" for field in _EVENT_FIELDS})
    detail.update(detail_extra)
    entry = {"EventBusName": bus, "Source": "elevate", "Detail": detail}
    if detail_type:
        entry["DetailType"] = detail_type
    else:
        entry["DetailType.$"] = "States.Format('elevate.request.{}', $.snap.grant.closeReason)"
    return {
        "Type": "Task",
        "Resource": _PUT_EVENTS,
        "Parameters": {"Entries": [entry]},
        "ResultPath": None,
        "Retry": _EVENT_RETRY,
        "Catch": [{"ErrorEquals": ["States.ALL"], "ResultPath": "$.eventError", "Next": nxt}],
        "Next": nxt,
    }


def _snapshot_into(path: str) -> dict:
    return {"ResultSelector": {"grant.$": "$.Payload.grant"}, "ResultPath": path}


def definition(engine_arn: str, bus_name: str) -> dict:
    e, b = engine_arn, bus_name
    revoke_result = {
        "ResultSelector": {
            "grant.$": "$.Payload.grant",
            "closeReason.$": "$.Payload.closeReason",
        },
        "ResultPath": "$.rev",
    }
    states = {
        "Snapshot": _engine(e, "snapshot", "EvCreated", **_snapshot_into("$.snap")),
        "EvCreated": _event(
            b, "$.snap.grant", "created", "elevate.request.created", "NeedsApproval"
        ),  # fmt: skip
        "NeedsApproval": {
            "Type": "Choice",
            "Choices": [
                {"Variable": "$.approval", "StringEquals": "manual", "Next": "WaitApproval"}
            ],
            "Default": "Activate",
        },
        "WaitApproval": _engine(
            e,
            "store_token",
            "AfterWait",
            wait=True,
            TimeoutSeconds=APPROVAL_TIMEOUT_SECONDS,
            ResultPath=None,
            # Timeout, or the wait could not start: the record decides in ResolveWait.
            Catch=[
                {"ErrorEquals": ["States.ALL"], "ResultPath": "$.waitError", "Next": "ResolveWait"}
            ],
        ),
        "ResolveWait": _engine(e, "resolve_wait", "AfterWait", ResultPath=None),
        "AfterWait": _engine(e, "snapshot", "Decision", **_snapshot_into("$.snap")),
        "Decision": {
            "Type": "Choice",
            "Choices": [
                {
                    "Variable": "$.snap.grant.state",
                    "StringEquals": "activating",
                    "Next": "EvApproved",
                },
                {
                    "Variable": "$.snap.grant.state",
                    "StringEquals": "closed",
                    "Next": "EvNotApproved",
                },
            ],
            # Still pending after a wait ended: let resolve_wait settle it.
            "Default": "ResolveWait",
        },
        "EvApproved": _event(
            b, "$.snap.grant", "approved", "elevate.request.approved", "Activate"
        ),  # fmt: skip
        "EvNotApproved": _event(b, "$.snap.grant", None, None, "Done"),
        "Activate": _engine(
            e,
            "activate",
            "EvActivated",
            ResultSelector={
                "grant.$": "$.Payload.grant",
                "waitSeconds.$": "$.Payload.waitSeconds",
            },
            ResultPath="$.act",
            Catch=[
                {"ErrorEquals": ["States.ALL"], "ResultPath": "$.activateError", "Next": "Revoke"}
            ],
        ),
        "EvActivated": _event(
            b, "$.act.grant", "activated", "elevate.grant.activated", "Active"
        ),  # fmt: skip
        # Ends on expiry (timeout) or on an early revocation (task success); both revoke.
        "Active": _engine(
            e,
            "store_token",
            "Revoke",
            wait=True,
            TimeoutSecondsPath="$.act.waitSeconds",
            ResultPath=None,
            Catch=[{"ErrorEquals": ["States.ALL"], "ResultPath": "$.activeEnd", "Next": "Revoke"}],
        ),
        "Revoke": _engine(
            e,
            "revoke",
            "EvClosed",
            **revoke_result,
            Catch=[
                {
                    "ErrorEquals": ["States.ALL"],
                    "ResultPath": "$.revokeError",
                    "Next": "MarkRevokeFailed",
                }
            ],
        ),
        "MarkRevokeFailed": _engine(e, "mark_revoke_failed", "EvError", **_snapshot_into("$.rev")),
        "EvError": _event(
            b,
            "$.rev.grant",
            "error",
            "elevate.grant.error",
            "RevokeAgain",
            errorCode="revoke_failed",
        ),  # fmt: skip
        # No Catch: the execution fails here, and a redrive resumes from this task
        # without running the activation again (design §6.5, spike C8).
        "RevokeAgain": _engine(e, "revoke", "EvClosed", **revoke_result),
        "EvClosed": _event(b, "$.rev.grant", "closed", "elevate.grant.closed", "Done"),
        "Done": {"Type": "Succeed"},
    }
    return {"Comment": "aws-elevate grant lifecycle", "StartAt": "Snapshot", "States": states}


def render(engine_arn: str, bus_name: str) -> str:
    return json.dumps(definition(engine_arn, bus_name), indent=2)
