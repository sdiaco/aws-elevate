"""Rules document: parsing with every constraint, what a caller can
request, and the coherence check the engine repeats before activating.

infra/rules.schema.json holds the same constraints for AppConfig, except the ones a
draft-04 schema cannot express (default <= max, unique ids, unique accounts)."""

import re
from dataclasses import dataclass

from .models import Account, Approval, Grant, Group, Identity, PermissionSetRef, Rule

SCHEMA_VERSION = 1
MIN_SECONDS = 300
MAX_SECONDS = 12 * 3600
PS_ARN = re.compile(r"^arn:aws[a-z-]*:sso:::permissionSet/ssoins-[0-9a-f]+/ps-[0-9a-f]+$")
_ACCOUNT_ID = re.compile(r"^[0-9]{12}$")
_DOC_FIELDS = ("schemaVersion", "rules")
_RULE_FIELDS = (
    "id",
    "group",
    "permissionSet",
    "accounts",
    "duration",
    "approval",
    "approverGroup",
    "privileged",
)


@dataclass(frozen=True)
class Problem:
    """One reason a rules document is refused. index and field say where (the rule
    position, a field of that rule such as "accounts" or "duration.maxSeconds"); the
    message is in plain words, so the UI can show it next to the form field."""

    index: int | None
    field: str | None
    message: str

    def __str__(self) -> str:
        if self.index is None:
            where = self.field
        else:
            where = f"rules[{self.index}]" + (f".{self.field}" if self.field else "")
        return f"{where}: {self.message}" if where else self.message


class InvalidRules(Exception):
    def __init__(self, problems: list[Problem]):
        self.problems = problems
        self.errors = [str(p) for p in problems]
        super().__init__("; ".join(self.errors))


class InvalidRequest(Exception):
    """A request the rule does not allow; the message tells the requester why."""


def parse(doc) -> list[Rule]:
    """All rules of a document, or InvalidRules listing every problem found."""
    if not isinstance(doc, dict):
        raise InvalidRules([Problem(None, None, "the document must be a JSON object")])
    problems = [Problem(None, None, f"unknown field {k}") for k in doc if k not in _DOC_FIELDS]
    if doc.get("schemaVersion") != SCHEMA_VERSION:
        problems.append(Problem(None, None, f"schemaVersion must be {SCHEMA_VERSION}"))
    items = doc.get("rules")
    if not isinstance(items, list):
        raise InvalidRules([*problems, Problem(None, None, "rules must be a list")])
    seen: dict[str, int] = {}
    for i, item in enumerate(items):
        rule_problems = [Problem(i, f, m) for f, m in _rule_errors(item)]
        problems += rule_problems
        if rule_problems:
            continue
        if seen.setdefault(item["id"], i) != i:
            problems.append(Problem(i, "id", f"another rule is already named {item['id']}"))
    if problems:
        raise InvalidRules(problems)
    return [rule_from_dict(item) for item in items]


# Each check below returns (field, message) pairs; field None means the whole rule.
def _rule_errors(item) -> list[tuple[str | None, str]]:
    if not isinstance(item, dict):
        return [(None, "must be an object")]
    missing = [f for f in _RULE_FIELDS if f not in item]
    if missing:
        return [(None, f"missing field {', '.join(missing)}")]
    errors = [(None, f"unknown field {k}") for k in item if k not in _RULE_FIELDS]
    if not _text(item["id"]):
        errors.append(("id", "enter a name"))
    errors += _ref_errors(item["group"], "group", ("id", "name"), "choose who can request")
    errors += _ref_errors(
        item["permissionSet"], "permissionSet", ("arn", "name"), "choose a permission set"
    )
    if not errors and not PS_ARN.match(item["permissionSet"]["arn"]):
        errors.append(("permissionSet.arn", "is not a permission set ARN"))
    errors += _account_errors(item["accounts"])
    errors += _duration_errors(item["duration"])
    errors += _approval_errors(item)
    return errors


def _text(value) -> bool:
    return isinstance(value, str) and value != ""


def _ref_errors(value, field: str, keys: tuple[str, ...], empty: str) -> list[tuple]:
    if not isinstance(value, dict) or set(value) != set(keys):
        return [(field, f"must be an object with exactly {', '.join(keys)}")]
    return [(field, empty)] if not all(_text(value[k]) for k in keys) else []


def _account_errors(value) -> list[tuple]:
    if not isinstance(value, list) or not value:
        return [("accounts", "choose at least one account")]
    errors, seen = [], set()
    for i, account in enumerate(value):
        where = f"accounts[{i}]"
        ref = _ref_errors(account, where, ("id", "name"), "needs an id and a name")
        if ref:
            errors += ref
        elif not _ACCOUNT_ID.match(account["id"]):
            errors.append((f"{where}.id", f"{account['id']} is not an account id (12 digits)"))
        elif account["id"] in seen:
            errors.append((f"{where}.id", f"account {account['id']} is listed twice"))
        else:
            seen.add(account["id"])
    return errors


def _seconds(value) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _duration_errors(value) -> list[tuple]:
    keys = ("defaultSeconds", "maxSeconds")
    if not isinstance(value, dict) or set(value) != set(keys):
        return [("duration", f"must be an object with exactly {', '.join(keys)}")]
    span = (
        f"must be from {MIN_SECONDS // 60} minutes to {MAX_SECONDS // 3600} hours "
        f"({MIN_SECONDS} to {MAX_SECONDS} seconds)"
    )
    errors = [
        (f"duration.{k}", span)
        for k in keys
        if not _seconds(value[k]) or not MIN_SECONDS <= value[k] <= MAX_SECONDS
    ]
    if not errors and value["defaultSeconds"] > value["maxSeconds"]:
        errors.append(("duration.defaultSeconds", "must not be longer than the maximum duration"))
    return errors


def _approval_errors(item: dict) -> list[tuple]:
    approval, approver = item["approval"], item["approverGroup"]
    if approval not in [a.value for a in Approval]:
        return [("approval", "must be auto or manual")]
    errors = []
    if approval == Approval.MANUAL:
        errors += _ref_errors(approver, "approverGroup", ("id", "name"), "choose who approves")
    elif approver is not None:
        errors.append(("approverGroup", "must be empty when approval is automatic"))
    if not isinstance(item["privileged"], bool):
        errors.append(("privileged", "must be true or false"))
    elif item["privileged"] and approval != Approval.MANUAL:
        errors.append(("privileged", "a privileged rule needs manual approval"))
    return errors


def requestable(rules: list[Rule], identity: Identity) -> list[Rule]:
    """Rules whose requester group the caller belongs to."""
    return [r for r in rules if r.group.id in identity.group_ids]


def validate_request(rule: Rule, account: str, duration_seconds: int) -> None:
    if account not in {a.id for a in rule.accounts}:
        raise InvalidRequest(f"account {account} is not allowed by rule {rule.id}")
    if not MIN_SECONDS <= duration_seconds <= rule.max_seconds:
        raise InvalidRequest(
            f"duration {duration_seconds}s must be between {MIN_SECONDS}s and "
            f"{rule.max_seconds}s for rule {rule.id}"
        )


def rule_from_dict(data: dict) -> Rule:
    approver = data.get("approverGroup")
    return Rule(
        id=data["id"],
        group=Group(**data["group"]),
        permission_set=PermissionSetRef(**data["permissionSet"]),
        accounts=tuple(Account(**a) for a in data["accounts"]),
        default_seconds=data["duration"]["defaultSeconds"],
        max_seconds=data["duration"]["maxSeconds"],
        approval=Approval(data["approval"]),
        approver_group=Group(**approver) if approver else None,
        privileged=data["privileged"],
    )


def _ref(ref: Group | Account) -> dict:
    return {"id": ref.id, "name": ref.name}


def rule_to_dict(rule: Rule) -> dict:
    """The rule as it is written in the rules document."""
    return {
        "id": rule.id,
        "group": _ref(rule.group),
        "permissionSet": {"arn": rule.permission_set.arn, "name": rule.permission_set.name},
        "accounts": [_ref(a) for a in rule.accounts],
        "duration": {"defaultSeconds": rule.default_seconds, "maxSeconds": rule.max_seconds},
        "approval": rule.approval.value,
        "approverGroup": _ref(rule.approver_group) if rule.approver_group else None,
        "privileged": rule.privileged,
    }


def find_rule(doc: dict, rule_id: str) -> Rule | None:
    for data in doc.get("rules", []):
        if data.get("id") == rule_id:
            return rule_from_dict(data)
    return None


def coherence_errors(grant: Grant, rule: Rule | None) -> list[str]:
    """Why the grant does not match the rule version it was created under. The record
    is ours, but the engine re-checks it before writing to Identity Center."""
    if rule is None:
        return [f"rule {grant.rule_id} is not in config version {grant.config_version}"]
    errors = []
    if grant.source_ps.arn != rule.permission_set.arn:
        errors.append(f"permission set {grant.source_ps.arn} is not the one of rule {rule.id}")
    if grant.account not in {a.id for a in rule.accounts}:
        errors.append(f"account {grant.account} is not allowed by rule {rule.id}")
    if grant.duration_seconds > rule.max_seconds:
        errors.append(f"duration {grant.duration_seconds}s exceeds the maximum {rule.max_seconds}s")
    if grant.approval != rule.approval:
        errors.append(f"approval {grant.approval} differs from rule approval {rule.approval}")
    if _group_id(grant.approver_group) != _group_id(rule.approver_group):
        errors.append(
            f"approver group {_group_name(grant.approver_group)} differs from the rule "
            f"approver group {_group_name(rule.approver_group)}"
        )
    if grant.privileged != rule.privileged:
        errors.append(f"privileged {grant.privileged} differs from rule {rule.privileged}")
    return errors


def _group_id(group: Group | None) -> str | None:
    return group.id if group else None


def _group_name(group: Group | None) -> str:
    return group.name if group else "none"
