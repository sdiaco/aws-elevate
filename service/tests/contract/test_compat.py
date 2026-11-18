import json
from pathlib import Path

import pytest

from aws_elevate.core.compat import Compat, check

CONTRACT = Path(__file__).resolve().parents[3] / "contract"
COMPAT = json.loads((CONTRACT / "compat.json").read_text())


def test_outcomes_match_the_contract():
    assert [c.value for c in Compat] == COMPAT["outcomes"]


@pytest.mark.parametrize("case", COMPAT["cases"], ids=lambda c: c["name"])
def test_every_contract_case(case):
    result = check(case["client"], case["infra"], case["minClient"], case["minInfra"])
    assert result == case["expected"]


@pytest.mark.parametrize("bad", ["", "1.2", "1.2.3.4", "v1.2.3", "1.x.3", "1.2.-3"])
def test_a_version_that_is_not_major_minor_patch_is_refused(bad):
    with pytest.raises(ValueError):
        check(bad, "0.0.1", "0.0.1", "0.0.1")
