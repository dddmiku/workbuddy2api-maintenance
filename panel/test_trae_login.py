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

    def test_public_callback_requires_https_and_safe_host(self):
        body = self.app_py
        start = body.index("def _trae_public_callback(")
        end = body.index("def trae_login_finish(", start)
        fn = body[start:end]
        # Host 头来自客户端：必须挡控制字符与路径分隔符（会被拼进发给 Trae 的 URL）。
        # 源码里写的是转义序列，断言按源码文本比对。
        self.assertIn(r'" \t\r\n/\\"', fn)
        self.assertIn("X-Forwarded-Proto", fn)
        self.assertIn("len(host) > 255", fn)

    def test_login_start_offers_fallback(self):
        """自动模式失败时仍要能手工粘贴——两条链接都给。"""
        body = self.app_py
        self.assertIn("fallback_url", body)
        self.assertIn("build_auth_url(login_trace_id, machine_id, device_id)", body)


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
        # 拼回调地址必须用 ADMIN_PREFIX，不能写死 /admin。
        start = body.index("def _trae_public_callback(")
        end = body.index("def trae_login_finish(", start)
        fn = body[start:end]
        self.assertIn("ADMIN_PREFIX", fn)
        self.assertNotIn('"/admin/trae/callback"', fn)

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


class TraeUIPrimaryPathTest(unittest.TestCase):
    """Trae 授权页以**手工路为主路径**，自动回调折叠为可选项。

    实测依据（2026-10-11）：授权页由字节 Passport SDK 驱动，我们用官方客户端的
    client_id，其回调白名单只认 localhost——把 auth_callback_url 指向本服务公网
    地址时 Trae 报「网络错误」，服务器上**一条回调请求都收不到**（nginx 访问日志
    里只有我自己的 curl 探测）。

    所以不能把走不通的那条摆在主路径上：让用户先点一条、失败了再回头找另一条，
    比一开始就给能走通的那条更糟。自动回调保留为折叠选项——自建 client_id 且登记了
    回调地址的部署可以用它一步完成。
    """

    def setUp(self):
        here = os.path.dirname(os.path.abspath(__file__))
        self.app_js = open(os.path.join(here, "app.js"), encoding="utf-8").read()
        self.index = open(os.path.join(here, "index.html"), encoding="utf-8").read()

    def test_manual_path_is_primary(self):
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                start = body.index("function renderTraeAuth()")
                end = body.index("function startTraeWatch()", start)
                block = body[start:end]
                # 主路径：打开授权页 + 剪贴板读取，两者都在折叠区之外。
                self.assertIn("drReopen", block)
                self.assertIn("drPaste", block)
                self.assertIn("127.0.0.1", block, "必须说清报错页是正常的")

    def test_manual_uses_localhost_link(self):
        """手工路必须用本机回调链接，否则浏览器不会停在带凭据的 127.0.0.1。"""
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                self.assertIn("DT.traeFallback || DT.url", body)

    def test_auto_callback_is_secondary(self):
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                self.assertIn("drAutoTry", body)
                start = body.index("function renderTraeAuth()")
                end = body.index("function startTraeWatch()", start)
                block = body[start:end]
                # 自动回调必须出现在 <details> 折叠块里，不能与主路径并列。
                self.assertIn("<details", block)
                self.assertLess(block.index("drPaste"), block.index("<details"),
                                "手工路要排在折叠区之前")

    def test_no_dead_render_function(self):
        """上一版留下的废弃函数要清掉，否则改一处漏一处。"""
        for name, body in (("app.js", self.app_js), ("index.html", self.index)):
            with self.subTest(name=name):
                self.assertNotIn("renderTraeWaitUnused", body)
                self.assertNotIn("renderTraeAuto", body)
