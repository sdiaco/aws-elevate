"""Rule routes of api (design §5.4, §7.1, §7.5, §11.2).

Only api reads the permission sets: ElevateRequester cannot read Identity Center, and the
limits of §5.4 depend on the permission set content, so the check before publishing lives
here, in one place, for both UI and CLI."""

import re
from datetime import UTC, datetime

from ..adapters.sso_read import PsNotFound
from ..core.ids import reserved_source
from ..core.models import Identity, Rule, SourcePs
from ..core.policy import build_inline, check_limits, summarize
from ..core.rules import PS_ARN, InvalidRules, parse, requestable, rule_to_dict
from .errors import ApiError, forbidden

# Codes of the entries of POST /rules/check, as in contract/vocabulary.json checkCodes.
CHECK_CODES = ("invalid", "ps_not_found", "limits", "reserved")
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
    if reserved_source(source.name):
        message = (
            f"permission set {source.name} is reserved to aws-elevate (JIT-* copies and "
            "Elevate* service sets): choose another one"
        )
        return [_entry(rule.id, "reserved", message)]
    # Same check the engine runs at activation (design §5.4).
    limits = check_limits(
        build_inline(source.inline, _SAMPLE_EXPIRY),
        len(source.managed) + len(source.customer_managed),
    )
    return [_entry(rule.id, "limits", f"{source.name}: {m}") for m in limits]


def _content(source: SourcePs) -> dict:
    return {
        "managed": list(source.managed),
        "customerManaged": [_ref(c) for c in source.customer_managed],
        "boundary": _boundary(source.boundary),
        "inline": source.inline,
        "inlineSummary": summarize(source.inline),
    }


def _reserved_account_errors(rule: Rule, reserved: frozenset[str]) -> list[dict]:
    # A grant in the management or the service account could tamper with aws-elevate
    # itself (design §7.5).
    return [
        _entry(
            rule.id,
            "reserved",
            f"account {a.id} ({a.name}) is reserved to aws-elevate (management or service "
            "account): manage it with Identity Center assignments",
        )
        for a in rule.accounts
        if a.id in reserved
    ]


class RuleRoutes:
    def __init__(self, config, sso, reserved_accounts: frozenset[str], table):
        self._config = config
        self._sso = sso
        self._reserved_accounts = reserved_accounts
        self._table = table

    def _visible(self, identity: Identity, rules: list[Rule]) -> list[Rule]:
        if identity.is_admin or identity.is_auditor:
            return rules
        return requestable(rules, identity)

    def _open_grants(self, identity: Identity, rules: list[Rule]) -> list[dict]:
        """Rule and account pairs a new request would be refused on as duplicate: the
        caller has an open grant there on the same permission set (design §11.2)."""
        held = {(g.source_ps.arn, g.account): g for g in self._table.open_of(identity.user_id)}
        out = []
        for rule in rules:
            for account in rule.accounts:
                g = held.get((rule.permission_set.arn, account.id))
                if g is not None:
                    out.append(
                        {
                            "ruleId": rule.id,
                            "account": account.id,
                            "grantId": g.id,
                            "state": g.state.value,
                        }
                    )
        return out

    def list(self, identity: Identity, query: dict) -> dict:
        """Admins and auditors see every rule; scope=requestable keeps only what the caller
        can request, for the Request view (design §11.2)."""
        if query not in ({}, {"scope": "requestable"}):
            raise ApiError(400, "invalid_request", "the only query is scope=requestable")
        version, rules = self._config.current()
        shown = requestable(rules, identity) if query else self._visible(identity, rules)
        return {
            "configVersion": version,
            "rules": [rule_to_dict(r) for r in shown],
            "openGrants": self._open_grants(identity, shown) if query else [],
        }

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
        return _content(source)

    def ps_permissions(self, identity: Identity, query: dict) -> dict:
        """What a permission set contains, for the Rules form before any rule uses it
        (design §7.5). Same answer as a rule's permissions; administrators only."""
        if not identity.is_admin:
            raise forbidden("only an administrator can read a permission set by ARN")
        arn = query.get("arn", "")
        if set(query) != {"arn"} or not PS_ARN.match(arn):
            raise ApiError(400, "invalid_request", "the only query is arn=<permission set ARN>")
        try:
            source = self._sso.read_source(arn)
        except PsNotFound as err:
            raise ApiError(
                404, "not_found", f"permission set {arn} does not exist in Identity Center"
            ) from err
        return _content(source)

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
            errors += _reserved_account_errors(rule, self._reserved_accounts)
            errors += _rule_errors(rule, sources[arn])
        return {"valid": not errors, "errors": errors}

    def _read(self, arn: str) -> SourcePs | None:
        try:
            return self._sso.read_source(arn)
        except PsNotFound:
            return None
