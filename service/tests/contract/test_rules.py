"""Every example in contract/rules/ gets the outcome its file name promises, from the
AppConfig schema and from the parser."""

import json
from pathlib import Path

import pytest
from jsonschema import Draft4Validator

from aws_elevate.core.rules import InvalidRules, parse

ROOT = Path(__file__).resolve().parents[3]
EXAMPLES = sorted((ROOT / "contract" / "rules").glob("*.json"))
SCHEMA = json.loads((ROOT / "infra" / "rules.schema.json").read_text())


def parser_accepts(doc) -> bool:
    try:
        parse(doc)
    except InvalidRules:
        return False
    return True


def test_schema_is_draft_04():
    # AWS AppConfig supports JSON Schema version 4 only (AppConfig user guide).
    assert SCHEMA["$schema"] == "http://json-schema.org/draft-04/schema#"
    Draft4Validator.check_schema(SCHEMA)


def test_every_kind_of_example_exists():
    prefixes = {p.name.split("-")[0] for p in EXAMPLES}
    assert prefixes == {"valid", "invalid", "parse"}


@pytest.mark.parametrize("path", EXAMPLES, ids=lambda p: p.name)
def test_example_outcome(path):
    doc = json.loads(path.read_text())
    schema_ok = Draft4Validator(SCHEMA).is_valid(doc)
    expected = {"valid": (True, True), "invalid": (False, False), "parse": (True, False)}
    assert (schema_ok, parser_accepts(doc)) == expected[path.name.split("-")[0]]
