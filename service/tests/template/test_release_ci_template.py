"""infra/release-ci.yaml: what the release workflow's role can and cannot do."""

from pathlib import Path

from cfnlint.decode import decode

TEMPLATE_PATH = Path(__file__).resolve().parents[3] / "infra" / "release-ci.yaml"
TEMPLATE, _errors = decode(str(TEMPLATE_PATH))
RESOURCES = TEMPLATE["Resources"]
ROLE = RESOURCES["ReleasePublisher"]["Properties"]


def as_list(value) -> list:
    return value if isinstance(value, list) else [value]


def statements() -> list[dict]:
    return [s for p in ROLE["Policies"] for s in p["PolicyDocument"]["Statement"]]


def test_template_decodes():
    assert not _errors


def test_only_main_of_the_repository_assumes_the_role():
    (trust,) = ROLE["AssumeRolePolicyDocument"]["Statement"]
    assert trust["Effect"] == "Allow"
    assert trust["Action"] == "sts:AssumeRoleWithWebIdentity"
    assert trust["Principal"] == {"Federated": {"Ref": "GitHubOidc"}}
    assert trust["Condition"] == {
        "StringEquals": {
            "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
            "token.actions.githubusercontent.com:sub": {
                "Fn::Sub": "repo:${Repository}:ref:refs/heads/main"
            },
        }
    }


def test_the_provider_is_github_for_sts():
    props = RESOURCES["GitHubOidc"]["Properties"]
    assert props["Url"] == "https://token.actions.githubusercontent.com"
    assert props["ClientIdList"] == ["sts.amazonaws.com"]


def test_the_role_writes_only_versioned_objects_of_the_release_buckets():
    actions = {a: s for s in statements() for a in as_list(s["Action"])}
    assert set(actions) == {"s3:PutObject", "s3:GetObject", "s3:ListBucket"}
    for statement in statements():
        assert statement["Effect"] == "Allow"
        assert "Condition" not in statement
    objects = {"Fn::Sub": "arn:${AWS::Partition}:s3:::${BucketPrefix}*/v*/*"}
    assert actions["s3:PutObject"]["Resource"] == objects
    assert actions["s3:GetObject"]["Resource"] == objects
    assert actions["s3:ListBucket"]["Resource"] == {
        "Fn::Sub": "arn:${AWS::Partition}:s3:::${BucketPrefix}*"
    }


def test_the_role_cannot_delete_or_change_bucket_settings():
    for statement in statements():
        for action in as_list(statement["Action"]):
            assert "Delete" not in action and "*" not in action
            assert not action.startswith("s3:PutBucket")


def test_no_other_resource_and_no_managed_policy():
    assert set(RESOURCES) == {"GitHubOidc", "ReleasePublisher"}
    assert "ManagedPolicyArns" not in ROLE
    assert "PermissionsBoundary" not in ROLE
