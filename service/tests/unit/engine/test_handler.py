import pytest

from aws_elevate.engine.handler import dispatch


class Recorder:
    def __init__(self):
        self.calls = []

    def snapshot(self, grant_id):
        self.calls.append(("snapshot", grant_id))
        return {"grant": {}}

    def store_token(self, grant_id, token):
        self.calls.append(("store_token", grant_id, token))
        return {"grant": {}}


def test_dispatch_calls_the_action():
    engine = Recorder()
    dispatch({"action": "snapshot", "grantId": "abcde23456"}, engine)
    assert engine.calls == [("snapshot", "abcde23456")]


def test_dispatch_passes_the_task_token():
    engine = Recorder()
    dispatch({"action": "store_token", "grantId": "abcde23456", "taskToken": "t"}, engine)
    assert engine.calls == [("store_token", "abcde23456", "t")]


@pytest.mark.parametrize(
    "event",
    [
        {"action": "activate", "grantId": "abcde23456", "account": "222222222222"},
        {"action": "snapshot", "grantId": "abcde23456", "taskToken": "t"},
        {"action": "store_token", "grantId": "abcde23456"},
        {"action": "drop_table", "grantId": "abcde23456"},
        {"action": "snapshot", "grantId": "../../etc"},
        {"action": "snapshot"},
    ],
)
def test_dispatch_rejects_anything_but_the_allowed_payload(event):
    engine = Recorder()
    with pytest.raises(ValueError):
        dispatch(event, engine)
    assert engine.calls == []


def test_handler_wires_the_engine_once_from_the_environment(monkeypatch):
    from aws_elevate.engine import handler as module
    from aws_elevate.engine.actions import Engine

    for name, value in {
        "AWS_DEFAULT_REGION": "eu-west-1",
        "INSTANCE_ARN": "arn:aws:sso:::instance/ssoins-1111111111111111",
        "TABLE_NAME": "elevate-grants",
        "APPCONFIG_APPLICATION_ID": "app1234",
        "APPCONFIG_PROFILE_ID": "prof123",
        "MANAGEMENT_ACCOUNT_ID": "999999999999",
        "SERVICE_ACCOUNT_ID": "888888888888",
    }.items():
        monkeypatch.setenv(name, value)
    monkeypatch.setattr(module, "_engine", None)
    with pytest.raises(ValueError):
        module.handler({"action": "nope"}, None)
    first = module._engine
    assert isinstance(first, Engine)
    assert first._reserved_accounts == {"999999999999", "888888888888"}
    with pytest.raises(ValueError):
        module.handler({"action": "nope"}, None)
    assert module._engine is first
