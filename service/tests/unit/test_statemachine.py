import ast
import json
import re
from pathlib import Path

import pytest

from aws_elevate.statemachine import definition, render

ENGINE = "arn:aws:lambda:eu-west-1:111111111111:function:elevate-engine"
BUS = "elevate-events"
SM = definition(ENGINE, BUS)
STATES = SM["States"]
TRANSITIONS = {"created", "approved", "activated", "closed", "error"}
ENGINE_ACTIONS = {
    "snapshot",
    "store_token",
    "resolve_wait",
    "activate",
    "revoke",
    "mark_revoke_failed",
}


def targets(state: dict) -> set[str]:
    out = set()
    if "Next" in state:
        out.add(state["Next"])
    if "Default" in state:
        out.add(state["Default"])
    out |= {c["Next"] for c in state.get("Choices", [])}
    out |= {c["Next"] for c in state.get("Catch", [])}
    return out


def engine_tasks():
    return {
        name: s
        for name, s in STATES.items()
        if s["Type"] == "Task" and s["Resource"].startswith("arn:aws:states:::lambda:invoke")
    }


def event_tasks():
    return {
        name: s
        for name, s in STATES.items()
        if s["Type"] == "Task" and s["Resource"] == "arn:aws:states:::events:putEvents"
    }


def test_every_target_exists_and_every_state_is_reachable():
    seen, todo = set(), [SM["StartAt"]]
    while todo:
        name = todo.pop()
        if name in seen:
            continue
        seen.add(name)
        assert name in STATES, name
        todo += targets(STATES[name])
    assert seen == set(STATES)


def test_no_fail_state_and_no_catch_into_one():
    # A Catch ending in a Fail state makes redrive accept and retry nothing.
    assert all(s["Type"] != "Fail" for s in STATES.values())


def test_revoke_again_has_retry_and_no_catch():
    again = STATES["RevokeAgain"]
    assert "Catch" not in again
    assert again["Retry"]
    assert again["Parameters"]["Payload"]["action"] == "revoke"


def test_revocation_failure_path():
    assert [c["Next"] for c in STATES["Revoke"]["Catch"]] == ["MarkRevokeFailed"]
    assert STATES["MarkRevokeFailed"]["Next"] == "EvError"
    assert STATES["EvError"]["Next"] == "RevokeAgain"
    assert STATES["RevokeAgain"]["Next"] == "EvClosed"
    assert STATES["Revoke"]["Next"] == "EvClosed"


def test_activation_failure_and_active_end_go_to_revoke():
    assert [c["Next"] for c in STATES["Activate"]["Catch"]] == ["Revoke"]
    assert STATES["Active"]["Next"] == "Revoke"
    catch = STATES["Active"]["Catch"]
    assert catch[0]["ErrorEquals"] == ["States.ALL"] and catch[0]["Next"] == "Revoke"


def test_waits():
    wait = STATES["WaitApproval"]
    assert wait["Resource"] == "arn:aws:states:::lambda:invoke.waitForTaskToken"
    assert wait["TimeoutSeconds"] == 1800
    assert [c["Next"] for c in wait["Catch"]] == ["ResolveWait"]
    active = STATES["Active"]
    assert active["Resource"] == "arn:aws:states:::lambda:invoke.waitForTaskToken"
    assert active["TimeoutSecondsPath"] == "$.act.waitSeconds"
    for name in ("WaitApproval", "Active"):
        payload = STATES[name]["Parameters"]["Payload"]
        assert payload["taskToken.$"] == "$$.Task.Token"


def test_record_decides_after_the_approval_wait():
    assert STATES["WaitApproval"]["Next"] == "AfterWait"
    assert STATES["ResolveWait"]["Next"] == "AfterWait"
    choices = STATES["Decision"]["Choices"]
    assert {c["Variable"] for c in choices} == {"$.snap.grant.state"}
    assert {c["StringEquals"]: c["Next"] for c in choices} == {
        "activating": "EvApproved",
        "closed": "EvNotApproved",
    }
    assert STATES["Decision"]["Default"] == "ResolveWait"


def test_engine_payload_carries_only_allowed_keys():
    for name, state in engine_tasks().items():
        payload = state["Parameters"]["Payload"]
        assert set(payload) <= {"action", "grantId.$", "taskToken.$"}, name
        assert payload["grantId.$"] == "$.grantId", name
        assert payload["action"] in ENGINE_ACTIONS, name
        assert state["Parameters"]["FunctionName"] == ENGINE


def test_engine_tasks_retry_retryable_errors():
    for name, state in engine_tasks().items():
        errors = {e for r in state["Retry"] for e in r["ErrorEquals"]}
        assert "Retryable" in errors, name
        assert "Lambda.TooManyRequestsException" in errors, name


def test_event_ids_are_grant_id_and_transition():
    pattern = re.compile(r"States\.Format\('\{\}:(\w+)', \$\.grantId\)")
    for name, state in event_tasks().items():
        detail = state["Parameters"]["Entries"][0]["Detail"]
        match = pattern.fullmatch(detail["eventId.$"])
        if name == "EvNotApproved":
            assert detail["eventId.$"] == (
                "States.Format('{}:{}', $.grantId, $.snap.grant.closeReason)"
            )
            continue
        assert match and match.group(1) in TRANSITIONS, name


def test_events_carry_the_contract_fields():
    fields = {
        "grantId", "requester", "approver", "approverGroup", "rule", "sourcePs", "account",
        "approval", "privileged", "startsAt", "expiresAt", "configVersion", "reason",
        "closeReason", "revokedBy", "revokeReason", "rejectReason", "error",
    }  # fmt: skip
    for name, state in event_tasks().items():
        entry = state["Parameters"]["Entries"][0]
        assert entry["EventBusName"] == BUS
        assert entry["Source"] == "elevate"
        detail = entry["Detail"]
        assert detail["schemaVersion"] == 1
        keys = {k.removesuffix(".$") for k in detail}
        assert fields <= keys, (name, fields - keys)


def test_detail_types():
    types = {n: s["Parameters"]["Entries"][0].get("DetailType") for n, s in event_tasks().items()}
    assert types == {
        "EvCreated": "elevate.request.created",
        "EvApproved": "elevate.request.approved",
        "EvNotApproved": None,
        "EvActivated": "elevate.grant.activated",
        "EvError": "elevate.grant.error",
        "EvClosed": "elevate.grant.closed",
    }
    not_approved = STATES["EvNotApproved"]["Parameters"]["Entries"][0]
    assert not_approved["DetailType.$"] == (
        "States.Format('elevate.request.{}', $.snap.grant.closeReason)"
    )


def test_error_event_says_revoke_failed():
    detail = STATES["EvError"]["Parameters"]["Entries"][0]["Detail"]
    assert detail["errorCode"] == "revoke_failed"


def test_a_lost_event_never_stops_the_grant_lifecycle():
    for name, state in event_tasks().items():
        assert state["Retry"], name
        assert [c["Next"] for c in state["Catch"]] == [state["Next"]], name


def test_render_is_json():
    assert json.loads(render(ENGINE, BUS)) == SM


# ---------- security boundary ----------

SRC = Path(__file__).resolve().parents[2] / "src" / "aws_elevate"


def module_name(path: Path) -> str:
    rel = path.relative_to(SRC.parent).with_suffix("")
    parts = list(rel.parts)
    if parts[-1] == "__init__":
        parts.pop()
    return ".".join(parts)


def imports_of(path: Path) -> set[str]:
    package = (
        module_name(path).rsplit(".", 1)[0] if path.name != "__init__.py" else module_name(path)
    )
    found = set()
    for node in ast.walk(ast.parse(path.read_text())):
        if isinstance(node, ast.Import):
            found |= {a.name for a in node.names}
        elif isinstance(node, ast.ImportFrom):
            if node.level:
                base = package.split(".")
                base = base[: len(base) - node.level + 1]
                prefix = ".".join(base + ([node.module] if node.module else []))
            else:
                prefix = node.module or ""
            found.add(prefix)
            found |= {f"{prefix}.{a.name}" for a in node.names}
    return {m for m in found if m.startswith("aws_elevate")}


def graph() -> dict[str, set[str]]:
    return {module_name(p): imports_of(p) for p in SRC.rglob("*.py")}


def reachable(start: str, edges: dict[str, set[str]]) -> set[str]:
    seen, todo = set(), [start]
    while todo:
        mod = todo.pop()
        if mod in seen:
            continue
        seen.add(mod)
        todo += [m for m in edges.get(mod, ()) if m in edges]
    return seen


def test_boundary_checker_sees_the_engine_importing_sso_admin():
    edges = graph()
    assert "aws_elevate.adapters.sso_admin" in reachable("aws_elevate.engine.handler", edges)


@pytest.mark.parametrize("module", sorted(m for m in graph() if m.startswith("aws_elevate.api")))
def test_api_never_reaches_sso_admin(module):
    assert "aws_elevate.adapters.sso_admin" not in reachable(module, graph())


def test_core_never_imports_boto3():
    for path in (SRC / "core").rglob("*.py"):
        for node in ast.walk(ast.parse(path.read_text())):
            names = []
            if isinstance(node, ast.Import):
                names = [a.name for a in node.names]
            elif isinstance(node, ast.ImportFrom):
                names = [node.module or ""]
            assert not any(n.split(".")[0] in ("boto3", "botocore") for n in names), path
