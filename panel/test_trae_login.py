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
