#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：验证限流写入与请求明细的真实面板鉴权、参数和私有转发，不把未知消费显示成零。

import http.client
import json
import threading
import unittest
from http.server import ThreadingHTTPServer
from unittest.mock import patch

import app


class FixtureHandler(app.Handler):
    def _session(self):
        return {"u": "fixture-admin"} if self.headers.get("Cookie") == "fixture-session" else None

    def log_message(self, *args):
        pass


class OpsManagementTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), FixtureHandler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def request(self, path, method="GET", body=None, session=True):
        headers = {"Content-Type": "application/json", "X-Admin-Request": "1"}
        if session:
            headers["Cookie"] = "fixture-session"
        else:
            headers["Authorization"] = "Bearer ordinary-fixture-key"
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=3)
        try:
            connection.request(method, path, None if body is None else json.dumps(body), headers)
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    def test_ops_are_admin_only(self):
        for path in ("/api/key-limits", "/api/requests", "/api/requests/req_fixture"):
            with self.subTest(path=path), patch.object(app.key_management, "request") as backend:
                self.assertEqual(self.request(path, session=False)[0], 401)
                backend.assert_not_called()

    def test_queries_forward_only_to_fixed_private_endpoints(self):
        for path, target in (
            ("/api/key-limits", "/key-limits"),
            ("/api/requests", "/requests"),
            ("/api/requests?model=global%3Ahy3&status=error&offset=0&limit=20", "/requests?model=global%3Ahy3&status=error&offset=0&limit=20"),
            ("/api/requests/req_fixture", "/requests/req_fixture"),
        ):
            with self.subTest(path=path), patch.object(app.key_management, "socket_path", return_value="fixture.sock"), patch.object(app.key_management, "request", return_value=(200, {"ok": True})) as backend:
                self.assertEqual(self.request(path)[0], 200)
                backend.assert_called_once_with("fixture.sock", "GET", target, None)

    def test_bad_queries_do_not_touch_private_transport(self):
        for path in (
            "/api/key-limits?path=/keys", "/api/requests?key_id=a&key_id=b", "/api/requests?secret=x",
            "/api/requests?limit=0", "/api/requests?limit=101", "/api/requests?offset=-1",
            "/api/requests?status=unknown", "/api/requests?model=%0d%0aX-Test%3Atrue",
            "/api/requests?model=%FF", "/api/requests/..%2fkeys", "/api/requests/%2Fkeys", "/api/requests/req_fixture?x=y",
        ):
            with self.subTest(path=path), patch.object(app.key_management, "request") as backend:
                self.assertEqual(self.request(path)[0], 400)
                backend.assert_not_called()

    def test_private_errors_keep_their_http_status(self):
        for status in (400, 404, 503):
            with self.subTest(status=status), patch.object(app.key_management, "socket_path", return_value="fixture.sock"), patch.object(app.key_management, "request", return_value=(status, {"ok": False, "message": "fixture"})):
                self.assertEqual(self.request("/api/requests/req_fixture")[0], status)

    def test_limits_create_update_and_explicit_clear(self):
        limits = {"requests_per_minute": 60, "max_concurrent": 2, "queue_timeout_seconds": 5}
        for path, body, method, endpoint, forwarded in (
            ("/api/keys", {"name": "fixture", "limits": limits}, "POST", "/keys", {"name": "fixture", "limits": limits}),
            ("/api/keys/update", {"id": "legacy", "limits": limits}, "PATCH", "/keys/legacy", {"limits": limits}),
            ("/api/keys/update", {"id": "legacy", "limits": None}, "PATCH", "/keys/legacy", {"limits": None}),
            ("/api/keys/update", {"id": "legacy", "name": "renamed"}, "PATCH", "/keys/legacy", {"name": "renamed"}),
        ):
            with self.subTest(body=body), patch.object(app.key_management, "socket_path", return_value="fixture.sock"), patch.object(app.key_management, "request", return_value=(200, {"ok": True})) as backend:
                self.assertEqual(self.request(path, "POST", body)[0], 200)
                backend.assert_called_once_with("fixture.sock", method, endpoint, forwarded)

    def test_invalid_limit_policy_is_not_silently_coerced(self):
        bad = [True, [], 1, {"typo": 1}, {"requests_per_minute": -1}, {"requests_per_minute": 60001},
               {"max_concurrent": 257}, {"queue_timeout_seconds": 31}, {"queue_timeout_seconds": 1},
               {"requests_per_minute": True}, {"max_concurrent": None}, {"max_concurrent": "2"}, {"max_concurrent": 2.5}]
        for limits in bad:
            with self.subTest(limits=limits), patch.object(app.key_management, "request") as backend:
                self.assertEqual(self.request("/api/keys/update", "POST", {"id": "legacy", "limits": limits})[0], 400)
                backend.assert_not_called()


if __name__ == "__main__":
    unittest.main()
