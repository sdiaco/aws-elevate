"""The end-to-end harness, without AWS: the guard against another installation and
the uninstall command that tears the test stack down."""

import importlib.util
import os
from pathlib import Path

import boto3
import pytest
from botocore.stub import Stubber

HARNESS = Path(__file__).resolve().parents[1] / "e2e" / "harness.py"
spec = importlib.util.spec_from_file_location("e2e_harness", HARNESS)
harness = importlib.util.module_from_spec(spec)
spec.loader.exec_module(harness)

INSTANCE = "arn:aws:sso:::instance/ssoins-1111111111111111"
ADMIN_PS = "arn:aws:sso:::permissionSet/ssoins-1111111111111111/ps-1111111111111111"


def _session() -> boto3.Session:
    return boto3.Session(aws_access_key_id="x", aws_secret_access_key="x", region_name="eu-west-1")


def _stub_instance_with(sso: Stubber, names: list[str]) -> None:
    sso.add_response(
        "list_instances",
        {"Instances": [{"InstanceArn": INSTANCE, "IdentityStoreId": "d-1111111111"}]},
    )
    arns = [f"{ADMIN_PS[:-1]}{i}" for i in range(len(names))]
    sso.add_response("list_permission_sets", {"PermissionSets": arns}, {"InstanceArn": INSTANCE})
    for arn, name in zip(arns, names, strict=True):
        sso.add_response(
            "describe_permission_set",
            {"PermissionSet": {"Name": name, "PermissionSetArn": arn}},
            {"InstanceArn": INSTANCE, "PermissionSetArn": arn},
        )


def test_up_stops_when_another_installation_exists(monkeypatch):
    session = _session()
    cfn, sso = session.client("cloudformation"), session.client("sso-admin")
    monkeypatch.setattr(harness, "sso_admin", lambda _s: sso)
    with Stubber(cfn) as cfn_stub, Stubber(sso) as sso_stub:
        cfn_stub.add_client_error(
            "describe_stacks", "ValidationError", "Stack with id elevate-e2e does not exist"
        )
        _stub_instance_with(sso_stub, ["AdministratorAccess", "ElevateAdmin"])
        with pytest.raises(SystemExit) as stop:
            harness.guard_other_installation(session, cfn)
    assert str(stop.value) == (
        "another Elevate installation uses this Identity Center instance: "
        "run e2e only in the dedicated test organization"
    )


def test_up_goes_on_without_another_installation(monkeypatch):
    session = _session()
    cfn, sso = session.client("cloudformation"), session.client("sso-admin")
    monkeypatch.setattr(harness, "sso_admin", lambda _s: sso)
    with Stubber(cfn) as cfn_stub, Stubber(sso) as sso_stub:
        cfn_stub.add_client_error(
            "describe_stacks", "ValidationError", "Stack with id elevate-e2e does not exist"
        )
        _stub_instance_with(sso_stub, ["AdministratorAccess"])
        harness.guard_other_installation(session, cfn)


def test_up_goes_on_when_the_installation_is_the_e2e_stack():
    session = _session()
    cfn = session.client("cloudformation")
    with Stubber(cfn) as cfn_stub:
        cfn_stub.add_response(
            "describe_stacks",
            {"Stacks": [{"StackName": "elevate-e2e", "StackStatus": "UPDATE_COMPLETE",
                         "CreationTime": "2026-11-18T00:00:00Z"}]},
        )  # fmt: skip
        harness.guard_other_installation(session, cfn)


def test_down_runs_uninstall_with_the_test_flags():
    cmd = harness.uninstall_command("eu-west-1", "diaco-test")
    assert cmd == [
        str(harness.CLI), "uninstall", "infra", "--profile", "elevate-e2e-install",
        "--elevate-profile", "diaco-test", "--region", "eu-west-1",
        "--stack-name", "elevate-e2e", "--history", "delete", "--yes",
    ]  # fmt: skip


def test_setup_command_points_at_the_service_account():
    assert harness.setup_command("diaco-test", "222222222222") == [
        str(harness.CLI), "setup", "--profile", "diaco-test", "--account", "222222222222",
    ]  # fmt: skip


def test_client_env_isolates_config_home_and_settings(tmp_path, monkeypatch):
    real_home = tmp_path / "home"
    (real_home / ".aws" / "sso" / "cache").mkdir(parents=True)
    (real_home / ".aws" / "config").write_text("[profile diaco-test]\nregion = eu-west-1\n")
    monkeypatch.setenv("HOME", str(real_home))
    monkeypatch.delenv("AWS_CONFIG_FILE", raising=False)
    monkeypatch.setenv("AWS_PROFILE", "other")
    monkeypatch.setenv("PROFILE", "diaco-test")
    monkeypatch.setenv("SERVICE_ACCOUNT", "222222222222")
    with harness.client_env("eu-west-1") as environ:
        home = Path(environ["HOME"])
        config = Path(environ["AWS_CONFIG_FILE"]).read_text()
        assert home != real_home
        assert environ["XDG_CONFIG_HOME"].startswith(str(home))
        assert "AWS_PROFILE" not in environ
        assert "[profile elevate-e2e-install]" in config
        assert "role_arn = arn:aws:iam::222222222222:role/OrganizationAccountAccessRole" in config
        assert "source_profile = diaco-test" in config
        assert (home / ".aws" / "sso").resolve() == (real_home / ".aws" / "sso").resolve()
    assert not home.exists()
    assert os.environ["HOME"] == str(real_home)


def _ddb_answering(error_code: str | None) -> tuple:
    ddb = _session().client("dynamodb")
    stub = Stubber(ddb)
    if error_code:
        stub.add_client_error("delete_table", error_code, "Table is being deleted")
    else:
        stub.add_response("delete_table", {})
    stub.activate()
    return ddb, stub


@pytest.mark.parametrize("code", ["ResourceInUseException", "ResourceNotFoundException"])
def test_down_counts_a_table_being_deleted_or_gone_as_done(code):
    # uninstall returns while DynamoDB still deletes the table.
    ddb, stub = _ddb_answering(code)
    failures: list[str] = []
    harness.delete_table(ddb, failures)
    assert failures == []
    stub.assert_no_pending_responses()


def test_down_reports_any_other_table_error():
    ddb, _ = _ddb_answering("AccessDeniedException")
    failures: list[str] = []
    harness.delete_table(ddb, failures)
    assert len(failures) == 1 and "AccessDeniedException" in failures[0]
