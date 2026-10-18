#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-02：锁定 growth_center 的 global 门控契约——global 账号不发起任何 CN 端点请求
#             （体检发现 20），与 task_runner / school 口径统一。
# 2026-10-18：补 trae 门控。实测 dev 上 31 个账号里 6 个 trae，被当成 CN 号去打
#             codebuddy.cn（必然 401）；新增 auth_realm / auth_skips_cn_task 并锁定。
import contextlib
import io
import os
import sys
import unittest
from unittest.mock import patch

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import growth_center as gc  # noqa: E402
import task_common as tc  # noqa: E402

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


class RealmClassificationTests(unittest.TestCase):
    """auth_realm 是脚本侧唯一的区域判定入口，必须与 Go auth.Realm() 同口径。

    Go 侧顺序（internal/auth/auth.go）：先判 RealmTrae → 再判 global → 否则 cn。
    """

    def test_trae_realm(self):
        self.assertEqual(tc.auth_realm({"realm": "trae"}), "trae")
        # 大小写与空白归一（手写 auth 文件常见）
        self.assertEqual(tc.auth_realm({"realm": " TRAE "}), "trae")
        # domain 是 trae 的真实落盘值；不能被误判成 cn 或 global
        self.assertEqual(
            tc.auth_realm({"realm": "trae", "domain": "https://api.trae.com.cn"}), "trae")

    def test_trae_is_neither_global_nor_cn(self):
        t = {"realm": "trae", "domain": "https://api.trae.com.cn"}
        self.assertFalse(tc.auth_is_global(t), "trae 不是 global")
        self.assertFalse(tc.auth_is_trae({"realm": "cn"}))
        self.assertTrue(tc.auth_is_trae(t))
        # 关键回归：trae 必须被判为「跳过 CN 任务」，否则会拿 Trae 的 JWT
        # 去打 codebuddy.cn（实测 6/31 个号每轮 401）
        self.assertTrue(tc.auth_skips_cn_task(t))

    def test_global_realm(self):
        self.assertEqual(tc.auth_realm({"realm": "global"}), "global")
        self.assertEqual(tc.auth_realm({"domain": "www.workbuddy.ai"}), "global")
        self.assertEqual(tc.auth_realm({"domain": "x.workbuddy.ai"}), "global")
        self.assertTrue(tc.auth_skips_cn_task({"realm": "global"}))
        self.assertFalse(tc.auth_is_trae({"realm": "global"}))

    def test_cn_realm(self):
        self.assertEqual(tc.auth_realm({"realm": "cn"}), "cn")
        self.assertEqual(tc.auth_realm({"domain": "www.codebuddy.cn"}), "cn")
        # 老凭证：realm 与 domain 都缺省 → cn（零回归）
        self.assertEqual(tc.auth_realm({}), "cn")
        self.assertFalse(tc.auth_skips_cn_task({"realm": "cn"}))
        self.assertFalse(tc.auth_is_global({}))

    def test_trae_wins_over_global_domain(self):
        # 显式 realm=trae 时，即使 domain 看着像别的域也按 trae 处理
        # （与 Go Realm() 里「先判 RealmTrae」同序）
        self.assertEqual(
            tc.auth_realm({"realm": "trae", "domain": "www.workbuddy.ai"}), "trae")


if __name__ == "__main__":
    unittest.main()
