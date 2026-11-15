"""Rules document (design §3.1, §7.1): parsing with every constraint, what a caller can
request, and the coherence check the engine repeats before activating (design §6.3).

infra/rules.schema.json holds the same constraints for AppConfig, except the ones a
draft-04 schema cannot express (default <= max, unique ids, unique accounts)."""

import re

from .models import Account, Approval, Grant, Group, Identity, PermissionSetRef, Rule

SCHEMA_VERSION = 1
MIN_SECONDS = 300
MAX_SECONDS = 12 * 3600
_PS_ARN = re.compile(r"^arn:aws[a-z-]*:sso:::permissionSet/ssoins-[0-9a-f]+/ps-[0-9a-f]+$")
_ACCOUNT_ID = re.compile(r"^[0-9]{12}$")
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


class InvalidRules(Exception):
    def __init__(self, errors: list[str]):
        super().__init__("; ".join(errors))
        self.errors = errors


class InvalidRequest(Exception):
    """A request the rule does not allow; the message tells the requester why."""


def parse(doc) -> list[Rule]:
    """All rules of a document, or InvalidRules listing every problem found."""
    if not isinstance(doc, dict):
        raise InvalidRules(["the document must be a JSON object"])
    errors = [f"unknown field {k}" for k in doc if k not in ("schemaVersion", "rules")]
    if doc.get("schemaVersion") != SCHEMA_VERSION:
        errors.append(f"schemaVersion must be {SCHEMA_VERSION}")
    items = doc.get("rules")
    if not isinstance(items, list):
        raise InvalidRules([*errors, "rules must be a list"])
    seen: dict[str, int] = {}
    for i, item in enumerate(items):
        where = f"rules[{i}]"
        rule_errors = _rule_errors(item, where)
        errors += rule_errors
        if rule_errors:
            continue
        first = seen.setdefault(item["id"], i)
        if first != i:
            errors.append(f"{where}.id: {item['id']} is already used by rules[{first}]")
    if errors:
        raise InvalidRules(errors)
    return [rule_from_dict(item) for item in items]


def _rule_errors(item, where: str) -> list[str]:
    if not isinstance(item, dict):
        return [f"{where}: must be an object"]
    missing = [f for f in _RULE_FIELDS if f not in item]
    if missing:
        return [f"{where}: missing field {', '.join(missing)}"]
    errors = [f"{where}: unknown field {k}" for k in item if k not in _RULE_FIELDS]
    if not _text(item["id"]):
        errors.append(f"{where}.id: must be a non-empty string")
    errors += _ref_errors(item["group"], f"{where}.group", ("id", "name"))
    errors += _ref_errors(item["permissionSet"], f"{where}.permissionSet", ("arn", "name"))
    if not errors and not _PS_ARN.match(item["permissionSet"]["arn"]):
        errors.append(f"{where}.permissionSet.arn: not a permission set ARN")
    errors += _account_errors(item["accounts"], f"{where}.accounts")
    errors += _duration_errors(item["duration"], f"{where}.duration")
    errors += _approval_errors(item, where)
    return errors


def _text(value) -> bool:
    return isinstance(value, str) and value != ""


def _ref_errors(value, where: str, keys: tuple[str, ...]) -> list[str]:
    if not isinstance(value, dict) or set(value) != set(keys):
        return [f"{where}: must be an object with exactly {', '.join(keys)}"]
    return [f"{where}.{k}: must be a non-empty string" for k in keys if not _text(value[k])]


def _account_errors(value, where: str) -> list[str]:
    if not isinstance(value, list) or not value:
        return [f"{where}: must be a non-empty list"]
    errors, seen = [], set()
    for i, account in enumerate(value):
        ref = _ref_errors(account, f"{where}[{i}]", ("id", "name"))
        if ref:
            errors += ref
        elif not _ACCOUNT_ID.match(account["id"]):
            errors.append(f"{where}[{i}].id: must be 12 digits")
        elif account["id"] in seen:
            errors.append(f"{where}[{i}].id: {account['id']} is listed twice")
        else:
            seen.add(account["id"])
    return errors


def _seconds(value) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _duration_errors(value, where: str) -> list[str]:
    keys = ("defaultSeconds", "maxSeconds")
    if not isinstance(value, dict) or set(value) != set(keys):
        return [f"{where}: must be an object with exactly {', '.join(keys)}"]
    errors = [
        f"{where}.{k}: must be a whole number of seconds between {MIN_SECONDS} and {MAX_SECONDS}"
        for k in keys
        if not _seconds(value[k]) or not MIN_SECONDS <= value[k] <= MAX_SECONDS
    ]
    if not errors and value["defaultSeconds"] > value["maxSeconds"]:
        errors.append(f"{where}.defaultSeconds: must not exceed maxSeconds")
    return errors


def _approval_errors(item: dict, where: str) -> list[str]:
    approval, approver = item["approval"], item["approverGroup"]
    if approval not in [a.value for a in Approval]:
        return [f"{where}.approval: must be auto or manual"]
    errors = []
    if approval == Approval.MANUAL:
        errors += _ref_errors(approver, f"{where}.approverGroup", ("id", "name"))
    elif approver is not None:
        errors.append(f"{where}.approverGroup: must be null when approval is auto")
    if not isinstance(item["privileged"], bool):
        errors.append(f"{where}.privileged: must be true or false")
    elif item["privileged"] and approval != Approval.MANUAL:
        errors.append(f"{where}.privileged: a privileged rule needs manual approval")
    return errors


def requestable(rules: list[Rule], identity: Identity) -> list[Rule]:
    """Rules whose requester group the caller belongs to (design §3.3)."""
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
