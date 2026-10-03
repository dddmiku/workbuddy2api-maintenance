#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-02：锁定 growth_center 的 global 门控契约——global 账号不发起任何 CN 端点请求
#             （体检发现 20），与 task_runner / school 口径统一。
import contextlib
import io
import os
import sys
import unittest
from unittest.mock import patch

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import growth_center as gc  # noqa: E402

UID = "abcdef1234567890abcdef1234567890"


class GlobalGateTests(unittest.TestCase):
    def test_global_account_makes_no_cn_request(self):
        # 回归（2026-10-02 第二轮体检发现 20）：growth_center 此前对 global 账号照发 CN
        # 端点（copilot.tencent.com），Go 侧 scheduler.go:674 明确这些端点对 global 无效。
        calls = []
        stats = {"accounts": 0, "redeem_ok": 0, "redeem_already": 0, "redeem_skip": 0,
                 "redeem_fail": 0, "makeup_ok": 0, "makeup_skip": 0, "makeup_fail": 0,
                 "won": 0, "draw_fail": 0, "streak_fail": 0}

        def boom(*a, **k):
            calls.append(a)
            raise AssertionError("global 账号不应发起任何 CN 请求")

        with patch.object(sys, "argv", ["growth_center.py", "fixture", "--redeem-only"]), \
             patch.object(gc, "collect_accounts", return_value=[{"uid": UID, "realm": "global"}]), \
             patch.object(gc.tc, "do_get", side_effect=boom), \
             patch.object(gc.tc, "do_post", side_effect=boom), \
             contextlib.redirect_stdout(io.StringIO()):
            try:
                gc.main()
            except SystemExit:
                pass
        self.assertEqual(calls, [])

    def test_global_detected_by_domain_too(self):
        self.assertTrue(gc.tc.auth_is_global({"realm": "global"}))
        self.assertTrue(gc.tc.auth_is_global({"domain": "www.workbuddy.ai"}))
        self.assertFalse(gc.tc.auth_is_global({"realm": "cn", "domain": "copilot.tencent.com"}))


if __name__ == "__main__":
    unittest.main()
