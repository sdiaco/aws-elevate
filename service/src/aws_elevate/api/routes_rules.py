"""Rule routes of api (design §5.4, §7.1, §7.5, §11.2).

Only api reads the permission sets: ElevateRequester cannot read Identity Center, and the
limits of §5.4 depend on the permission set content, so the check before publishing lives
here, in one place, for both UI and CLI."""

import re
from datetime import UTC, datetime

from ..adapters.sso_read import PsNotFound
from ..core.models import Identity, Rule, SourcePs
from ..core.policy import build_inline, check_limits, summarize
from ..core.rules import InvalidRules, parse, requestable, rule_to_dict
from .errors import ApiError, forbidden

# Codes of the entries of POST /rules/check, as in contract/vocabulary.json checkCodes.
CHECK_CODES = ("invalid", "ps_not_found", "limits")
# Only the length of the expiry statement matters for the limits, and every timestamp
# has the same length.
_SAMPLE_EXPIRY = datetime(2000, 1, 1, tzinfo=UTC)
_RULE_INDEX = re.compile(r"rules\[(\d+)\]")


def _ref(entry: dict) -> dict:
    return {"name": entry["Name"], "path": entry["Path"]}


def _boundary(boundary: dict | None) -> dict | None:
    if boundary is None:
        return None
    if "ManagedPolicyArn" in boundary:
        return {"managedPolicyArn": boundary["ManagedPolicyArn"]}
    return {"customerManagedPolicy": _ref(boundary["CustomerManagedPolicyReference"])}


def _rule_id_of(error: str, doc: dict) -> str | None:
    """Id of the rule a parser error is about, when the rule has a readable id."""
    match = _RULE_INDEX.match(error)
    if not match:
        return None
    item = doc["rules"][int(match.group(1))]
    rule_id = item.get("id") if isinstance(item, dict) else None
    return rule_id if isinstance(rule_id, str) else None


def _entry(rule_id: str | None, code: str, message: str) -> dict:
    return {"ruleId": rule_id, "code": code, "message": message}


def _rule_errors(rule: Rule, source: SourcePs | None) -> list[dict]:
    if source is None:
        message = (
            f"permission set {rule.permission_set.name} ({rule.permission_set.arn}) "
            "does not exist in Identity Center"
        )
        return [_entry(rule.id, "ps_not_found", message)]
    # Same check the engine runs at activation (design §5.4).
    limits = check_limits(
        build_inline(source.inline, _SAMPLE_EXPIRY),
        len(source.managed) + len(source.customer_managed),
    )
    return [_entry(rule.id, "limits", f"{source.name}: {m}") for m in limits]


class RuleRoutes:
    def __init__(self, config, sso):
        self._config = config
        self._sso = sso

    def _visible(self, identity: Identity, rules: list[Rule]) -> list[Rule]:
        if identity.is_admin or identity.is_auditor:
            return rules
        return requestable(rules, identity)

    def list(self, identity: Identity, query: dict) -> dict:
        """Admins and auditors see every rule; scope=requestable keeps only what the caller
        can request, for the Request view (design §11.2)."""
        if query not in ({}, {"scope": "requestable"}):
            raise ApiError(400, "invalid_request", "the only query is scope=requestable")
        version, rules = self._config.current()
        shown = requestable(rules, identity) if query else self._visible(identity, rules)
        return {"configVersion": version, "rules": [rule_to_dict(r) for r in shown]}

    def permissions(self, identity: Identity, rule_id: str) -> dict:
        _, rules = self._config.current()
        rule = next((r for r in rules if r.id == rule_id), None)
        if rule is None:
            raise ApiError(404, "not_found", f"no rule {rule_id}")
        # Approvers read what they grant even when they cannot request it (design §11.2).
        approver = rule.approver_group is not None and rule.approver_group.id in identity.group_ids
        if not approver and rule not in self._visible(identity, rules):
            raise forbidden(f"rule {rule_id} is not one you can request or approve")
        try:
            source = self._sso.read_source(rule.permission_set.arn)
        except PsNotFound as err:
            raise ApiError(
                404,
                "not_found",
                f"permission set {rule.permission_set.name} of rule {rule_id} no longer "
                "exists in Identity Center; ask an administrator",
            ) from err
        return {
            "managed": list(source.managed),
            "customerManaged": [_ref(c) for c in source.customer_managed],
            "boundary": _boundary(source.boundary),
            "inline": source.inline,
            "inlineSummary": summarize(source.inline),
        }

    def check(self, identity: Identity, body) -> dict:
        if not identity.is_admin:
            raise forbidden("only an administrator can check rules")
        if not isinstance(body, dict) or set(body) != {"doc"}:
            raise ApiError(400, "invalid_request", 'the body must be {"doc": <rules document>}')
        doc = body["doc"]
        try:
            rules = parse(doc)
        except InvalidRules as err:
            errors = [_entry(_rule_id_of(e, doc), "invalid", e) for e in err.errors]
            return {"valid": False, "errors": errors}
        errors = []
        sources: dict[str, SourcePs | None] = {}
        for rule in rules:
            arn = rule.permission_set.arn
            if arn not in sources:
                sources[arn] = self._read(arn)
            errors += _rule_errors(rule, sources[arn])
        return {"valid": not errors, "errors": errors}

    def _read(self, arn: str) -> SourcePs | None:
        try:
            return self._sso.read_source(arn)
        except PsNotFound:
            return None
