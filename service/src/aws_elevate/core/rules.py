"""Rules document (design §3.1, §7.1). Full parsing and validation arrive with the api;
the engine only needs to rebuild a rule and check a grant against it (design §6.3)."""

from .models import Account, Approval, Grant, Group, PermissionSetRef, Rule


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
    if grant.privileged != rule.privileged:
        errors.append(f"privileged {grant.privileged} differs from rule {rule.privileged}")
    return errors
