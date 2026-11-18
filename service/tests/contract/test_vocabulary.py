import json
from pathlib import Path

import pytest

from aws_elevate.core.models import Approval, CloseReason, Role, State

CONTRACT = Path(__file__).resolve().parents[3] / "contract"
VOCABULARY = json.loads((CONTRACT / "vocabulary.json").read_text())
COMPAT = json.loads((CONTRACT / "compat.json").read_text())


@pytest.mark.parametrize(
    ("key", "enum"),
    [
        ("states", State),
        ("closeReasons", CloseReason),
        ("approvals", Approval),
        ("roles", Role),
    ],
)
def test_enum_values_match_vocabulary(key, enum):
    assert [member.value for member in enum] == VOCABULARY[key]


def test_error_codes_are_unique_snake_case():
    codes = VOCABULARY["errorCodes"]
    assert len(codes) == len(set(codes))
    assert all(code.replace("_", "").isalpha() and code.islower() for code in codes)


def test_compat_cases_are_well_formed():
    outcomes = set(COMPAT["outcomes"])
    for case in COMPAT["cases"]:
        assert set(case) == {"name", "client", "infra", "minClient", "minInfra", "expected"}
        assert case["expected"] in outcomes
    assert {case["expected"] for case in COMPAT["cases"]} == outcomes
