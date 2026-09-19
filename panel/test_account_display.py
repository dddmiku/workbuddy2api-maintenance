#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-20：锁定账号表「状态/最近活动」两列的口径——冷却中必须显示冷却剩余与原因，
#             不能把凭证有效期（剩 N 天）摆在冷却旁边；只有错误记录的账号要显示
#             「无成功记录 + 最近错误」，不能再显示「从未」（用户实测反馈）。

import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
INDEX = os.path.join(HERE, "index.html")


def read(name):
    with open(os.path.join(HERE, name), "r", encoding="utf-8") as fh:
        return fh.read()


class AccountDisplayTests(unittest.TestCase):
    def setUp(self):
        self.app_js = read("app.js")
        self.app_py = read("app.py")
        self.index = read("index.html")

    def test_state_exposes_cooldown_ledger(self):
        # 面板需要冷却剩余、冷却类别、原因与累计计数才能渲染状态列。
        for field in ("coolRemaining", "coolKind", "reason", "successCount", "errTotal"):
            with self.subTest(field=field):
                self.assertIn('"%s"' % field, self.app_py)

    def test_cooldown_helpers_exist_and_are_used(self):
        self.assertIn("function coolLeft(sec)", self.app_js)
        self.assertIn("function coolReason(p)", self.app_js)
        self.assertIn("coolLeft(p.coolRemaining)", self.app_js)
        self.assertIn("coolReason(p)", self.app_js)

    def test_status_cell_prefers_cooldown_over_credential_expiry(self):
        # 冷却中必须走冷却分支；凭证有效期只对非冷却号展示。
        self.assertIn("p.cooling ? (coolLeft(p.coolRemaining)", self.app_js)
        self.assertIn("'凭证剩 '", self.app_js)

    def test_last_activity_distinguishes_no_success_from_no_record(self):
        self.assertIn("'无成功记录'", self.app_js)
        self.assertIn("'最近错误 '", self.app_js)

    def test_built_index_carries_the_helpers(self):
        # 浏览器实际加载的是生成后的 index.html，规则必须真的拼进去。
        self.assertIn("function coolLeft(sec)", self.index)
        self.assertIn("function coolReason(p)", self.index)
        self.assertIn("'无成功记录'", self.index)


if __name__ == "__main__":
    unittest.main()
