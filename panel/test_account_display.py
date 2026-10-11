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
        self.assertIn("var coolSub = p.cooling", self.app_js)
        self.assertIn("esc(coolLeft(p.coolRemaining)", self.app_js)
        self.assertIn("'凭证剩 '", self.app_js)

    def test_last_activity_distinguishes_no_success_from_no_record(self):
        self.assertIn("'无成功记录'", self.app_js)
        self.assertIn("'最近错误 '", self.app_js)

    def test_built_index_carries_the_helpers(self):
        # 浏览器实际加载的是生成后的 index.html，规则必须真的拼进去。
        self.assertIn("function coolLeft(sec)", self.index)
        self.assertIn("function coolReason(p)", self.index)
        self.assertIn("'无成功记录'", self.index)

    def test_cooldown_rows_carry_a_live_countdown(self):
        # 冷却中的行必须带截止时间戳，交给每秒 tick 就地刷新；否则用户只能等下次
        # 整页拉取才看到剩余时间变化。
        self.assertIn("function startCoolTicker()", self.app_js)
        self.assertIn("function tickCooldowns()", self.app_js)
        self.assertIn("data-cool-end=", self.app_js)
        self.assertIn("startCoolTicker();", self.app_js)
        self.assertIn("data-cool-end=", self.index)

    def test_minute_scale_cooldown_uses_clock_format(self):
        # 一小时内的冷却显示 mm:ss，秒数每秒可见变化。
        self.assertIn("(ss < 10 ? '0' : '') + ss", self.app_js)

    def test_countdown_reloads_once_when_it_hits_zero(self):
        # 归零后补取一次数据，让「冷却中」翻成可用；不能每秒重拉。
        self.assertIn("COOL_RELOAD_PENDING", self.app_js)
        self.assertIn("if (document.hidden) return;", self.app_js)


class RealmCapabilityTest(unittest.TestCase):
    """锁定「域能力」字段：trae 域没有 WorkBuddy 的积分接口。

    2026-10-11 接入 trae 时发现，面板把 ./credit 对非 WorkBuddy 账号的「跳过」
    说明当成查询结果展示，于是 trae 账号在账号页与积分页显示成一片错误。
    后端现在显式给出 credits.supported，前端据此显示「不适用」。
    """

    def _state(self, realm):
        import app as panel_app
        from unittest import mock
        summary = {"uid": "u1", "realm": realm, "nickname": "n", "disabled": False,
                   "expiresAt": 0, "domain": "", "enterpriseId": "", "parsable": True}
        with mock.patch.object(panel_app, "list_auth_files",
                               return_value=[{"uid": "u1", "file": "f.json",
                                              "disabled": False, "path": "p"}]), \
             mock.patch.object(panel_app, "auth_summary", return_value=summary), \
             mock.patch.object(panel_app, "gateway_get", return_value={}), \
             mock.patch.object(panel_app, "credits_by_uid",
                               return_value=({}, {"accounts": []})), \
             mock.patch.object(panel_app, "container_running", return_value=True), \
             mock.patch.object(panel_app, "_expiring_soon_env", return_value=""):
            return panel_app.build_state()

    def test_trae_account_reports_its_own_credits(self):
        """trae 有**自己的**积分接口（pay API），不再显示为「不适用」。

        2026-10-11 实测 /trae/api/v2/pay/ide_user_ent_usage 可用，额度换算成整数
        走同一套字段。单位与 WorkBuddy 积分不同，前端按 realm 标注、不做跨域合计。
        """
        acct = self._state("trae")["accounts"][0]
        self.assertIs(acct["credits"]["supported"], True)

    def test_all_realms_report_credits(self):
        for realm in ("cn", "global", "trae"):
            acct = self._state(realm)["accounts"][0]
            self.assertIs(acct["credits"]["supported"], True, realm)


if __name__ == "__main__":
    unittest.main()
