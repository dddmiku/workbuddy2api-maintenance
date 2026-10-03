#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-02：锁定登录全局节流的 429 响应完整性——此前把 {"Retry-After": n} 字典
#             当 extra 头列表传给 _send，字典被迭代成键字符串导致解包 ValueError，
#             响应头未收尾并多吐一个 500。

import re
import socket
import threading
import unittest
from unittest.mock import patch

import app
from test_security_boundaries import QuietServer, SessionHandler


class LoginThrottleResponseTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = QuietServer(("127.0.0.1", 0), SessionHandler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join(timeout=3)

    def raw_login(self, body=b'{"username":"admin","password":"secret"}'):
        """用裸 socket 取原始字节：需要看到「一个请求是不是吐了两个响应」。"""
        connection = socket.create_connection(self.server.server_address, timeout=3)
        try:
            request = (b"POST /api/auth/login HTTP/1.1\r\n"
                       b"Host: 127.0.0.1\r\n"
                       b"Content-Type: application/json\r\n"
                       b"X-Admin-Request: 1\r\n"
                       b"Connection: close\r\n"
                       b"Content-Length: " + str(len(body)).encode("ascii") + b"\r\n\r\n" + body)
            connection.sendall(request)
            chunks = []
            while True:
                data = connection.recv(4096)
                if not data:
                    break
                chunks.append(data)
            return b"".join(chunks)
        finally:
            connection.close()

    def test_throttled_login_returns_exactly_one_well_formed_429(self):
        with patch.object(app, "login_global_retry_after", return_value=42):
            raw = self.raw_login()

        statuses = re.findall(rb"HTTP/1\.1 (\d{3})", raw)
        self.assertEqual(statuses, [b"429"],
                         "被节流的一次登录必须只产生一个响应：%r" % raw[:400])
        self.assertIn(b"Retry-After: 42", raw)
        self.assertIn(b"\r\n\r\n", raw)

        head, _, body = raw.partition(b"\r\n\r\n")
        self.assertTrue(body, "429 响应体为空：%r" % raw[:400])
        self.assertIn(b'"ok": false', body)
        # 不能把 Python 异常原文回显给客户端。
        self.assertNotIn(b"unpack", body)
        self.assertNotIn(b"Traceback", body)


if __name__ == "__main__":
    unittest.main()
