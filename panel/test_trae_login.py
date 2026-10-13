#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-11：初版——Trae 授权回调解析与落盘形态的回归测试。
"""trae_login 的回归测试。

重点覆盖「用户粘贴进来的东西形态不定」这一实际风险：完整回调 URL、裸查询串、
只有 refreshToken，三种都要能解析；解析不出来时必须报错而不是写一个空账号。
"""

import json
import os
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import trae_login  # noqa: E402


def jwt_query(token="jwt-token", refresh="refresh-token", uid="12345", screen="tester"):
    user_jwt = json.dumps({"Token": token, "RefreshToken": refresh, "ClientID": "ono9krqynydwx5"})
    user_info = json.dumps({"UserID": uid, "ScreenName": screen, "Region": "CN"})
    return "userJwt=%s&userInfo=%s" % (
        urllib_quote(user_jwt), urllib_quote(user_info))


def urllib_quote(value):
    import urllib.parse
    return urllib.parse.quote(value, safe="")


class ParseCallbackTest(unittest.TestCase):

    def test_full_callback_url(self):
        url = "http://127.0.0.1:8765/authorize?" + jwt_query()
        creds = trae_login.parse_callback(url)
        self.assertEqual(creds["access_token"], "jwt-token")
        self.assertEqual(creds["refresh_token"], "refresh-token")
        self.assertEqual(creds["uid"], "12345")
        self.assertEqual(creds["nickname"], "tester")

    def test_bare_query_string(self):
        creds = trae_login.parse_callback(jwt_query())
        self.assertEqual(creds["access_token"], "jwt-token")
        self.assertEqual(creds["uid"], "12345")

    def test_query_with_leading_question_mark(self):
        creds = trae_login.parse_callback("?" + jwt_query())
        self.assertEqual(creds["access_token"], "jwt-token")

    def test_surrounding_whitespace(self):
        creds = trae_login.parse_callback("  \n http://127.0.0.1:8765/authorize?" + jwt_query() + "\n  ")
        self.assertEqual(creds["access_token"], "jwt-token")

    def test_old_flow_exchanges_refresh_token(self):
        """老流程回调只带 refreshToken：必须走 ExchangeToken 换取 JWT。"""
        def fake_exchange(refresh_token, client_id=None, host=""):
            self.assertEqual(refresh_token, "legacy-refresh")
            return {
                "token": "exchanged-jwt",
                "refreshToken": "rotated-refresh",
                "tokenExpireAt": "1791700000",
                "userInfo": {"UserID": "999", "ScreenName": "legacy"},
                "host": "https://api.trae.cn",
            }

        with mock.patch.object(trae_login, "exchange_refresh_token", side_effect=fake_exchange):
            creds = trae_login.parse_callback(
                "http://127.0.0.1:8765/authorize?refreshToken=legacy-refresh")
        self.assertEqual(creds["access_token"], "exchanged-jwt")
        self.assertEqual(creds["refresh_token"], "rotated-refresh")
        self.assertEqual(creds["uid"], "999")

    def test_bare_refresh_token_string(self):
        with mock.patch.object(trae_login, "exchange_refresh_token",
                               return_value={"token": "jwt2", "userInfo": {"UserID": "7"}}):
            creds = trae_login.parse_callback("abcdefghijklmnopqrstuvwxyz012345")
        self.assertEqual(creds["access_token"], "jwt2")

    def test_empty_input_fails(self):
        with self.assertRaises(RuntimeError):
            trae_login.parse_callback("")
        with self.assertRaises(RuntimeError):
            trae_login.parse_callback("   ")

    def test_garbage_input_fails(self):
        with self.assertRaises(RuntimeError):
            trae_login.parse_callback("这不是一个链接")

    def test_no_token_fails(self):
        """有查询串但没有 userJwt/refreshToken：必须报错，不能写出空账号。"""
        with self.assertRaises(RuntimeError):
            trae_login.parse_callback("http://127.0.0.1:8765/authorize?foo=bar")


class AuthUrlTest(unittest.TestCase):

    def test_url_shape(self):
        url = trae_login.build_auth_url("trace-1", "machine-1", "1234567890123456789")
        self.assertTrue(url.startswith("https://www.trae.cn/authorization?"))
        self.assertIn("client_id=ono9krqynydwx5", url)
        self.assertIn("login_trace_id=trace-1", url)
        self.assertIn("auth_callback_url=http%3A%2F%2F127.0.0.1%3A8765%2Fauthorize", url)
        self.assertIn("auth_type=local", url)


class NormalizeExpireTest(unittest.TestCase):

    def test_seconds(self):
        self.assertEqual(trae_login.normalize_expire("1791700000"), 1791700000)

    def test_milliseconds(self):
        self.assertEqual(trae_login.normalize_expire("1791700000000"), 1791700000)

    def test_unparsable(self):
        self.assertEqual(trae_login.normalize_expire(""), 0)
        self.assertEqual(trae_login.normalize_expire("not-a-number"), 0)
        self.assertEqual(trae_login.normalize_expire(None), 0)


class WriteTraeAuthFileTest(unittest.TestCase):
    """落盘形态必须能被网关的 auth.Parse 读回并判成 trae 域。"""

    def test_written_document_shape(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            import app
            original = app.AUTHS_DIR
            app.AUTHS_DIR = tmp
            try:
                summary = app.write_trae_auth_file(
                    {"access_token": "jwt", "refresh_token": "rt",
                     "nickname": "trae-user", "host": "https://api.trae.cn"},
                    "12345", 1791700000)
            finally:
                app.AUTHS_DIR = original

            path = os.path.join(tmp, "workbuddy-trae-12345.json")
            self.assertTrue(os.path.exists(path), os.listdir(tmp))
            with open(path, encoding="utf-8") as handle:
                doc = json.loads(handle.read())
            self.assertEqual(doc["auth"]["realm"], "trae")
            self.assertEqual(doc["auth"]["accessToken"], "jwt")
            self.assertEqual(doc["account"]["uid"], "12345")
            self.assertEqual(summary["realm"], "trae")

    def test_filename_matches_gateway_glob(self):
        """文件名必须匹配网关的 workbuddy*.json，否则账号根本不会被扫描到。"""
        import fnmatch
        name = "workbuddy-trae-12345.json"
        self.assertTrue(fnmatch.fnmatch(name, "workbuddy*.json"), name)

    def test_missing_access_token_refused(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            import app
            original = app.AUTHS_DIR
            app.AUTHS_DIR = tmp
            try:
                with self.assertRaises(RuntimeError):
                    app.write_trae_auth_file({"refresh_token": "rt"}, "12345", 1791700000)
            finally:
                app.AUTHS_DIR = original
            self.assertEqual(os.listdir(tmp), [])


if __name__ == "__main__":
    unittest.main()


class RealCallbackShapeTest(unittest.TestCase):
    """锁定 2026-10-11 实测的真实回调形态。

    用户实测的回调是**老流程**：地址栏带 refreshToken（不是 userJwt），
    且同时带一个 `data` 参数。这里钉住"优先取 refreshToken"这一选择——
    如果哪天改成优先取 data，这条测试会立刻失败。
    """

    REAL = ("http://127.0.0.1:8765/authorize?isRedirect=true&scope=solo"
            "&data=sJWEae4XXVAvUxBRp5zMfVrJazeYFO1-JiyaYP3-E%3D%3D.18dd695b7837d5a9"
            "&refreshToken=sJWEae4XXVAvUxBRp5zMfVrJazeYFO1-JiyaYP3-E%3D%3D.18dd695b7837d5a9")

    def test_real_callback_prefers_refresh_token_over_data(self):
        captured = {}

        def fake_exchange(refresh_token, client_id=None, host=""):
            captured["rt"] = refresh_token
            return {"token": "JWT", "userInfo": {"UserID": "42", "ScreenName": "u"}}

        with mock.patch.object(trae_login, "exchange_refresh_token", side_effect=fake_exchange):
            creds = trae_login.parse_callback(self.REAL)

        self.assertEqual(captured["rt"],
                         "sJWEae4XXVAvUxBRp5zMfVrJazeYFO1-JiyaYP3-E==.18dd695b7837d5a9")
        self.assertEqual(creds["access_token"], "JWT")
        self.assertEqual(creds["uid"], "42")

    def test_authorization_page_link_is_rejected_clearly(self):
        """把「授权页」链接贴回来是走错一步：必须明确说明，而不是去换 token。"""
        url = trae_login.build_auth_url("t", "m", "1")
        with self.assertRaises(RuntimeError) as ctx:
            trae_login.parse_callback(url)
        self.assertIn("授权页", str(ctx.exception))
        self.assertIn("127.0.0.1", str(ctx.exception))

    def test_bare_token_with_base64_chars(self):
        """裸 token 含 + / = 等字符，不能被当成解析失败。"""
        token = "sJWEae4XXVAvUxBRp5zMfVrJazeYFO1-JiyaYP3-E==.18dd695b7837d5a9"
        with mock.patch.object(trae_login, "exchange_refresh_token",
                               return_value={"token": "JWT", "userInfo": {"UserID": "1"}}):
            creds = trae_login.parse_callback(token)
        self.assertEqual(creds["access_token"], "JWT")

    def test_human_sentence_rejected(self):
        """明显不是 token 的文本必须报错，不能被当成裸 token 送去兑换。"""
        with self.assertRaises(RuntimeError):
            trae_login.parse_callback("我登录好了，你看看")


class CallbackRouteTest(unittest.TestCase):
    """锁定公网回调这条「一键授权」路径。

    Trae 授权完成后浏览器直接跳到本服务，**没有管理台会话 cookie**——回调路由
    必须在会话检查之前处理，否则会被重定向到登录页、把凭据参数丢掉。
    """

    def setUp(self):
        here = os.path.dirname(os.path.abspath(__file__))
        with open(os.path.join(here, "app.py"), "r", encoding="utf-8") as fh:
            self.app_py = fh.read()

    def test_callback_handled_before_session_check(self):
        body = self.app_py
        cb = body.index("if path == TRAE_CALLBACK_PATH:")
        # 会话拒绝分支（session 为空 → 401/重定向）必须在回调之后。
        guard = body.index("if not session:", cb)
        self.assertLess(cb, guard,
                        "callback must be routed before the session gate")

    def test_callback_page_never_echoes_credentials(self):
        """回调页经公网暴露，绝不能把 token 回显到页面上。"""
        body = self.app_py
        start = body.index("def trae_callback_page(")
        end = body.index("def escape_html(", start)
        page = body[start:end]
        for secret in ("access_token", "refresh_token", "accessToken", "token"):
            self.assertNotIn(secret, page,
                             "callback page must not render %s" % secret)

    def test_login_start_builds_localhost_link(self):
        """登录入口只生成本机回调链接——Trae 唯一接受的回调地址。"""
        self.assertIn("build_auth_url(login_trace_id, machine_id, device_id)", self.app_py)


class TraeAuthUrlCallbackTest(unittest.TestCase):

    def test_custom_callback_is_used(self):
        url = trae_login.build_auth_url("t", "m", "1",
                                        "https://example.com/admin/trae/callback")
        self.assertIn("auth_callback_url=https%3A%2F%2Fexample.com%2Fadmin%2Ftrae%2Fcallback", url)

    def test_default_callback_is_localhost(self):
        url = trae_login.build_auth_url("t", "m", "1")
        self.assertIn("auth_callback_url=http%3A%2F%2F127.0.0.1%3A8765%2Fauthorize", url)


class AdminPrefixPortabilityTest(unittest.TestCase):
    """锁定面板前缀可配——这是**公开发布**必须的。

    面板被嵌在网关的一个子路径下，这个前缀同时决定：
      1. 网关路由注册（/admin/ → 面板）
      2. 代理剥前缀
      3. 面板拼**公网回调地址**（Trae 授权回调要能被外部访问到）

    写死 "/admin" 会让别的部署形态（挂在 /panel、或直接挂在根）下 Trae 回调
    指向一个不存在的地址。2026-10-11 用户指出这一点时，三处里有两处是写死的。
    """

    def test_panel_reads_prefix_from_env(self):
        here = os.path.dirname(os.path.abspath(__file__))
        with open(os.path.join(here, "app.py"), "r", encoding="utf-8") as fh:
            body = fh.read()
        self.assertIn("WB2API_ADMIN_PREFIX", body)
        self.assertIn("ADMIN_PREFIX", body)
        # 公网回调函数已移除（Trae 不接受它）；回调路由与常量仍在。
        self.assertNotIn("_trae_public_callback", body)
        self.assertIn("TRAE_CALLBACK_PATH", body)

    def test_prefix_normalization(self):
        """前缀归一化：补前导斜杠、去尾斜杠、空值回落 /admin。"""
        import importlib
        cases = [("", "/admin"), ("/admin", "/admin"), ("panel", "/panel"),
                 ("/panel/", "/panel"), ("/", "/admin"), ("  /x/y  ", "/x/y")]
        for raw, want in cases:
            with self.subTest(raw=raw):
                os.environ["WB2API_ADMIN_PREFIX"] = raw
                try:
                    mod = importlib.reload(trae_login_app())
                    self.assertEqual(mod.ADMIN_PREFIX, want)
                finally:
                    os.environ.pop("WB2API_ADMIN_PREFIX", None)
                    importlib.reload(trae_login_app())


def trae_login_app():
    import app
    return app


class TraeUIManualOnlyTest(unittest.TestCase):
    """Trae 授权页只有**手工路**，自动回调已按用户实测反馈移除。

    依据（2026-10-11 实测）：授权页由字节 Passport SDK 驱动，我们用官方客户端的
    client_id，其回调白名单只认 localhost。把 auth_callback_url 指向本服务公网地址
    时 Trae 一律报「网络错误」，服务器上一条回调请求都收不到（nginx 访问日志可证）。
    用户反馈「你的自动全是网络错误」——所以界面上不再提供它，避免再让人白试一次。
    """

    def setUp(self):
        here = os.path.dirname(os.path.abspath(__file__))
        self.app_js = open(os.path.join(here, "app.js"), encoding="utf-8").read()
        self.index = open(os.path.join(here, "index.html"), encoding="utf-8").read()
        self.app_py = open(os.path.join(here, "app.py"), encoding="utf-8").read()

    def test_manual_path_present(self):
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                start = body.index("function renderTraeAuth()")
                block = body[start:body.index("async function submitTraeCallback()", start)]
                self.assertIn("drReopen", block, "打开授权页")
                self.assertIn("drPaste", block, "从剪贴板读取")
                self.assertIn("127.0.0.1", block, "必须说清报错页是正常的")

    def test_manual_uses_localhost_link(self):
        """必须用本机回调链接——它是 Trae 唯一接受的回调地址。

        2026-10-13：改用真正的 `<a href=DT.url>` 而不是 `window.open(...)`。
        原因是手机 App 的 WebView 没有地址栏，而 Trae 流程要求用户复制地址栏里的
        回调链接；`<a target="_blank">` 的导航一定会经过 WebView 的
        shouldOverrideUrlLoading，由 App 交给系统浏览器，行为比 window.open 确定。
        所以这里断言的是「用的是 DT.url 那条链接」，不再绑定具体的打开方式。
        """
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                self.assertIn('href="\' + esc(authURL) + \'"', body)
                self.assertIn("var authURL = DT.url || ''", body)
                # 链接必须来自后端给的那条本机回调地址，不能是别处拼出来的。
                self.assertNotIn("_trae_public_callback", body)

    def test_no_auto_callback_ui_left(self):
        """自动回调的 UI 必须彻底移除，不留半截入口。"""
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                for gone in ("drAutoTry", "startTraeWatch", "renderTraeAuto",
                             "renderTraeWaitUnused", "traeAuto"):
                    self.assertNotIn(gone, body, "%s should be gone" % gone)

    def test_backend_only_builds_localhost_callback(self):
        """后端也不再生成公网回调链接（生成了也没人用，徒增误解）。"""
        start = self.app_py.index("def trae_login_start(")
        block = self.app_py[start:self.app_py.index("def trae_login_finish(", start)]
        self.assertIn("build_auth_url(login_trace_id, machine_id, device_id)", block)
        self.assertNotIn("_trae_public_callback()", block)


class TaskRealmGroupingTest(unittest.TestCase):
    """任务页按**账号区域**分组，分组依据是后端给的 t.realms。

    2026-10-11 用户要求「任务点按照实际账号区域分类」。依据必须是任务实际作用的域，
    不能按名字猜——签到同时覆盖 cn/global/trae 三个域，其余任务只服务 WorkBuddy 两域。
    """

    def setUp(self):
        here = os.path.dirname(os.path.abspath(__file__))
        self.app_js = open(os.path.join(here, "app.js"), encoding="utf-8").read()
        self.index = open(os.path.join(here, "index.html"), encoding="utf-8").read()

    def test_groups_by_realm_field(self):
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                start = body.index("function renderTasks(){")
                block = body[start:body.index("function taskTile(", start)]
                self.assertIn("t.realms", block, "分组必须读 realms 字段")
                self.assertIn("realmTag(realm)", block, "分组标题用域标签")

    def test_only_renders_realms_with_accounts(self):
        """只渲染池里真有账号的域：没有 trae 号时列「Trae」分组会让人以为漏了配置。

        该判据在 v2.10.1 挪进了 taskRealms()——顶部区域切换要用同一份域列表，
        所以把它从 renderTasks 里抽出来共用（两处各写一份必然漂移）。
        """
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                self.assertIn("function taskRealms(", body)
                start = body.index("function taskRealms(")
                block = body[start:body.index("\n}", start)]
                self.assertIn("present[r]", block)
                # renderTasks 必须复用那一份，不能自己再算一遍。
                rt = body[body.index("function renderTasks(){"):]
                rt = rt[:rt.index("function taskTile(")]
                self.assertIn("taskRealms(list)", rt)

    def test_overview_no_longer_shows_schedule(self):
        """概览页不再有「今日排程」卡片（用户要求去掉）。"""
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                self.assertNotIn("今日排程", body)
                self.assertNotIn("renderOvTasks", body)
                self.assertNotIn("btnGoTasks", body)

    def test_sidebar_says_tasks_not_schedule(self):
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                self.assertIn("任务", body)
                self.assertNotIn("排程任务</b>", body)


class PanelTaskWhitelistTest(unittest.TestCase):
    """面板的任务白名单必须覆盖网关的全部任务。

    2026-10-11 用户实测：Trae 签到任务在面板上显示正常，但点「跑一次」报
    「任务标识不正确」——网关侧任务已存在，只是 app.py 的 TASK_ENABLE_KEY
    白名单漏登记了新任务。它同时闸住 /api/task/run、/api/task/toggle 与
    /api/task/log 三条路径，漏一个就等于这个任务在面板上只能看不能用。
    """

    def test_whitelist_covers_every_gateway_task(self):
        import re
        here = os.path.dirname(os.path.abspath(__file__))
        with open(os.path.join(here, "app.py"), encoding="utf-8") as fh:
            panel = fh.read()
        # 从网关源码里取权威任务清单（TaskKeyXxx TaskKey = "xxx"）。
        src = os.path.join(here, "..", "internal", "scheduler", "tasks.go")
        with open(os.path.normpath(src), encoding="utf-8") as fh:
            go = fh.read()
        want = set(re.findall(r'TaskKey\w+\s+TaskKey\s*=\s*"([a-z_]+)"', go))
        self.assertTrue(want, "未能从 tasks.go 解析出任务清单")
        have = set(re.findall(r'^\s{4}"([a-z_]+)":\s*"([a-z_]+)_enabled",', panel, re.M))
        have = {k for k, _ in have}
        missing = sorted(want - have)
        self.assertEqual(missing, [],
                         "面板白名单缺少这些任务（会导致「跑一次」报任务标识不正确）：%s" % missing)

    def test_every_task_has_an_enable_switch(self):
        """每个任务的开关字段名必须是 <key>_enabled，与 config 段一致。"""
        import re
        here = os.path.dirname(os.path.abspath(__file__))
        with open(os.path.join(here, "app.py"), encoding="utf-8") as fh:
            panel = fh.read()
        block = panel[panel.index("TASK_ENABLE_KEY = {"):]
        block = block[:block.index("}")]
        pairs = re.findall(r'"([a-z_]+)":\s*"([a-z_]+)",', block)
        self.assertTrue(pairs, "未解析到 TASK_ENABLE_KEY")
        for key, field in pairs:
            self.assertEqual(field, key + "_enabled",
                             "%s 的开关字段应为 %s_enabled，实际 %s" % (key, key, field))
