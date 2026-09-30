#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-30：锁定脚本侧的 auth 文件契约——宽 glob（workbuddy*.json，与网关 AuthFileGlob 同口径）
#             与扁平形 auth 文件（审查发现 8 / 19）。此前 load_auth 只认嵌套形，且各脚本各自
#             拼窄 glob，导致网关在服务的账号让整轮调度 KeyError 崩溃或被漏掉。
import json
import os
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import task_common as tc  # noqa: E402

UID = "abcdef1234567890abcdef1234567890"


def nested_doc():
    return {
        "auth": {"accessToken": "tok-nested", "refreshToken": "ref", "expiresAt": 1893456000,
                 "domain": "copilot.tencent.com", "realm": "cn"},
        "account": {"uid": UID, "nickname": "NestedGuy", "enterpriseId": "ent"},
    }


def flat_doc():
    return {"accessToken": "tok-flat", "refreshToken": "ref", "expiresAt": 1893456000,
            "uid": UID, "nickname": "FlatGuy", "realm": "cn", "domain": "copilot.tencent.com"}


class AuthFileContractTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.auths = self.dir.name
        p = patch.object(tc, "AUTHS", self.auths)
        p.start()
        self.addCleanup(p.stop)

    def write(self, name, doc):
        with open(os.path.join(self.auths, name), "w", encoding="utf-8") as fh:
            json.dump(doc, fh, ensure_ascii=False, indent=1)

    def test_nested_form_loads(self):
        self.write("workbuddy-%s.json" % UID, nested_doc())
        a = tc.load_auth(UID)
        self.assertEqual(a["token"], "tok-nested")
        self.assertEqual(a["uid"], UID)
        self.assertEqual(a["nick"], "NestedGuy")
        self.assertEqual(a["realm"], "cn")

    def test_flat_form_loads(self):
        # 回归（审查发现 8）：扁平形此前在 d["auth"] 处抛未捕获的 KeyError，整轮调度挂掉。
        self.write("workbuddy-%s.json" % UID, flat_doc())
        a = tc.load_auth(UID)
        self.assertEqual(a["token"], "tok-flat")
        self.assertEqual(a["uid"], UID)
        self.assertEqual(a["nick"], "FlatGuy")
        self.assertEqual(a["realm"], "cn")

    def test_flat_form_without_realm_defaults_cn(self):
        doc = flat_doc()
        doc.pop("realm")
        self.write("workbuddy-%s.json" % UID, doc)
        self.assertEqual(tc.load_auth(UID)["realm"], "")

    def test_missing_required_field_raises_systemexit_not_keyerror(self):
        # 单个损坏文件必须走 SystemExit（调用方统一 except），而不是 KeyError 整轮崩溃。
        self.write("workbuddy-%s.json" % UID, {"refreshToken": "ref"})
        with self.assertRaises(SystemExit):
            tc.load_auth(UID)

    def test_all_auth_files_uses_wide_glob(self):
        # 回归（审查发现 19）：无连字符的名字网关会加载，脚本 ALL 枚举也必须看到。
        self.write("workbuddy-%s.json" % UID, nested_doc())
        self.write("workbuddy_new.json", flat_doc())
        self.write("workbuddy.json", flat_doc())
        names = [os.path.basename(p) for p in tc.all_auth_files()]
        self.assertEqual(names, ["workbuddy-%s.json" % UID, "workbuddy.json", "workbuddy_new.json"])

    def test_all_auth_files_skips_disabled(self):
        # .disabled 结尾不是 .json，与网关「禁用账号不加载」一致。
        self.write("workbuddy-%s.json" % UID, nested_doc())
        self.write("workbuddy-disabled.json.disabled", nested_doc())
        self.assertEqual([os.path.basename(p) for p in tc.all_auth_files()],
                         ["workbuddy-%s.json" % UID])

    def test_prefix_lookup_matches_hyphenless_file(self):
        # 按文件名前缀找号同样要覆盖宽命名空间：此前 glob 是 workbuddy-{pre}*.json，
        # 对 workbuddy_new.json 这类无连字符的文件永远匹配不到。
        self.write("workbuddy_new.json", flat_doc())
        self.assertEqual(tc.load_auth("new")["uid"], UID)


if __name__ == "__main__":
    unittest.main()
