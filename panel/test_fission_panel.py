#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-12：初版——面板「补发邀请奖励」按钮：只对 Trae 域出现、只传 uid、
#             失败时保留上游原文、native 模式的参数白名单必须窄。

import os
import re
from pathlib import Path
import unittest

from test_frontend_behavior import FrontendHarness


PANEL = Path(__file__).resolve().parent


def read(name):
    with open(PANEL / name, "r", encoding="utf-8") as fh:
        return fh.read()


class FissionPanelTest(unittest.TestCase):
    """面板补发邀请奖励的接线。

    为什么按 uid 放在账号行上、而不是任务页：邀请关系是账号级事实，补发是
    一次性动作，不是可排程的周期任务。任务页的每张卡片都隐含「会按小时重复跑」，
    放那儿会误导。
    """

    def setUp(self):
        self.app_js = read("app.js")
        self.app_py = read("app.py")
        self.native = read("native_runtime.py")
        self.index = read("index.html")

    def test_button_only_for_trae_accounts(self):
        """按钮必须只在 trae 域出现：WorkBuddy 两域没有 code fission 端点。

        无条件渲染会让 cn/global 账号点出「没有匹配的 Trae 账号」，看着像故障。
        """
        self.assertIn("data-fission=", self.app_js)
        # 渲染处必须带 realm === 'trae' 的判断。
        start = self.app_js.index("data-fission=")
        head = self.app_js[max(0, start - 400):start]
        self.assertIn("realmOf(a) === 'trae'", head,
                      "补发按钮必须限定在 trae 域")
        self.assertIn("data-fission=", self.index, "生成的 index.html 里也要有")

    def test_click_handler_is_wired(self):
        """按钮必须有对应的点击分支，否则点了没反应。"""
        self.assertIn("[data-fission]", self.app_js)
        self.assertIn("actFission(", self.app_js)
        self.assertRegex(self.app_js, r"function actFission\(uid\)")

    def test_action_posts_uid_only(self):
        """请求体只带 uid——设备号代数由后端定，前端不该能指定。"""
        start = self.app_js.index("function actFission(uid)")
        body = self.app_js[start:start + 900]
        self.assertIn("api/account/fission", body)
        self.assertIn("uid:uid", body)
        self.assertNotIn("gen", body, "前端不得传设备号代数")

    def test_endpoint_validates_uid_and_shape(self):
        """接口必须校验 uid，且字段白名单只有 uid。"""
        self.assertIn('"/api/account/fission": {"uid"}', self.app_py)
        self.assertIn("def account_fission(self, body)", self.app_py)
        start = self.app_py.index("def account_fission(self, body)")
        body = self.app_py[start:start + 1400]
        self.assertIn("UID_RE.fullmatch(uid)", body, "必须校验 uid 形态")
        self.assertIn("container_running()", body, "容器没跑时不能假装成功")

    def test_endpoint_runs_the_helper_by_uid(self):
        """必须走 ./fission_bin 且只传 -uid。"""
        start = self.app_py.index("def account_fission(self, body)")
        body = self.app_py[start:start + 1400]
        self.assertIn('"./fission_bin"', body)
        self.assertIn('"-uid"', body)

    def test_failure_keeps_upstream_message(self):
        """失败时必须把上游原文透出来。

        上游用业务码区分原因（1027 未被邀请 / 9070 暂无可领取权益 / 9091 设备判重），
        吞掉它就没法排障。
        """
        start = self.app_py.index("def fission_message(row)")
        body = self.app_py[start:start + 1200]
        self.assertIn("row.get(\"error\")", body, "失败分支要带上游 error 原文")
        for state in ("granted", "already", "failed"):
            self.assertIn(state, body, "三态都要有文案")

    def test_native_runtime_allowlist_is_narrow(self):
        """原生模式没有容器边界，参数白名单必须窄。

        只允许 -uid <数字> 与 -gen <非负整数>；-auths 等一律拒绝（由
        native_runtime 自己注入账号目录）。
        """
        self.assertIn("_fission_args_ok", self.native)
        self.assertIn("'./fission_bin'", self.native)
        # 校验函数本身的行为。
        ns = {"re": re}
        src = self.native
        start = src.index("def _fission_args_ok(args):")
        end = src.index("\ndef run(args", start)
        exec(compile(src[start:end], "native_runtime.py", "exec"), ns)
        check = ns["_fission_args_ok"]
        self.assertTrue(check([]), "空参数合法（全量扫描）")
        self.assertTrue(check(["-uid", "2081842786229369"]))
        self.assertTrue(check(["-gen", "0"]))
        self.assertTrue(check(["-uid", "123", "-gen", "7"]))
        # 拒绝：路径穿越、注入额外旗标、非数字 uid、负数 gen。
        self.assertFalse(check(["-auths", "/etc"]), "不得指定账号目录")
        self.assertFalse(check(["-uid", "../../etc/passwd"]))
        self.assertFalse(check(["-uid", "123", "-pretty"]), "不得注入未允许旗标")
        self.assertFalse(check(["-uid"]), "缺值必须拒绝")
        self.assertFalse(check(["-gen", "-1"]), "负数代数必须拒绝")
        self.assertFalse(check(["-uid", "1; rm -rf /"]), "不得带 shell 元字符")

    def test_index_rebuilt_with_button(self):
        """生成的 index.html 必须真的包含新按钮（面板是拼接产物）。"""
        self.assertIn("data-fission=", self.index)
        self.assertIn("function actFission(uid)", self.index)


if __name__ == "__main__":
    unittest.main()


class FissionButtonRendersTest(FrontendHarness, unittest.TestCase):
    """按钮必须在真实渲染路径里出现——只查源码字符串会漏掉「代码在但走不到」。

    2026-10-12 用户实测「没看到按钮」。源码里 data-fission 确实存在，所以源码级
    断言全绿，但按钮没渲染出来。这类问题只有把 renderAccounts 真正跑一遍才抓得到：
    账号行渲染函数里任何一处提前 return / 条件写错，源码字符串都照样在。
    """

    def setUp(self):
        import re as _re
        self.app_js = read("app.js")

    def test_realm_guard_uses_realmOf_like_the_rest_of_the_file(self):
        """必须用 realmOf(a) 而不是 a.realm。

        后端对「account 段没有 realm」的账号只下发 auth.realm（实测 trae 账号
        account.realm 为 None、auth.realm 为 'trae'），全文件其余地方一律走
        realmOf() 兜底（缺失按 cn 处理）。只有这一处写裸 a.realm 会与全文件口径
        不一致——虽然当前后端恰好下发了 'trae'，但裸比较是脆的。
        """
        start = self.app_js.index("data-fission=")
        head = self.app_js[max(0, start - 400):start]
        self.assertIn("realmOf(a) === 'trae'", head,
                      "补发按钮必须与全文件同口径地用 realmOf(a)")
        self.assertNotIn("(a.realm === 'trae'", head,
                         "不得裸比较 a.realm")

    def test_row_render_actually_emits_the_button(self):
        """真正跑一遍账号行渲染，断言按钮出现在 HTML 里。

        用 Node 加载 app.js，构造一个 trae 账号喂给渲染函数。这比字符串匹配强：
        字符串匹配无法发现「函数提前返回」「条件恒假」这类问题。
        """
        result = self.run_frontend("""
function render1(a){
  S.data = {accounts:[a]};
  S.filter = 'all'; S.realm = ''; S.q = '';
  renderAccounts();
  return $('#acctRows').innerHTML;
}
var trae = render1({uid:'1571669390934250', nickname:'trae 用户', realm:'trae',
            disabled:false, expiresAt:0, credits:{remain:4500,size:4500,used:0},
            pool:{inPool:true, healthy:true, cooling:false}});
var cn = render1({uid:'u1', nickname:'cn 用户', realm:'cn', disabled:false, expiresAt:0,
          credits:{remain:100,size:100,used:0}, pool:{inPool:true, healthy:true, cooling:false}});
return { trae: trae, cn: cn };
""")
        self.assertIn("data-fission=", result["trae"],
                      "trae 账号行必须渲染出补发按钮")
        self.assertIn("补发邀请", result["trae"])
        self.assertNotIn("data-fission=", result["cn"],
                         "cn 账号不得出现补发按钮")

    def test_realm_missing_falls_back_like_backend(self):
        """realm 缺失时按 cn 处理（与后端 `or "cn"` 同口径），不出按钮。"""
        result = self.run_frontend("""
var a = {uid:'x1', nickname:'no realm', disabled:false, expiresAt:0,
         credits:{remain:0,size:0,used:0}, pool:{}};
S.data = {accounts:[a]}; S.filter='all'; S.realm=''; S.q='';
renderAccounts();
return { html: $('#acctRows').innerHTML, realm: realmOf(a) };
""")
        self.assertEqual(result["realm"], "cn")
        self.assertNotIn("data-fission=", result["html"])
