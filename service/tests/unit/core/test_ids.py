import re

from aws_elevate.core.ids import new_grant_id, ps_name


def test_grant_id_is_ten_lowercase_base32_characters():
    for _ in range(200):
        assert re.fullmatch(r"[a-z2-7]{10}", new_grant_id())


def test_grant_ids_differ():
    assert len({new_grant_id() for _ in range(1000)}) == 1000


def test_ps_name_prefixes_grant_id():
    assert ps_name("abcde23456") == "JIT-abcde23456"
