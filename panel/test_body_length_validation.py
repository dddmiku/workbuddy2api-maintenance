#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-02：锁定 _body 的 Content-Length 必须按 ASCII 数字严格校验——此前用
#             str.isdigit() 放行了 latin-1 数字字节（如 0xB2），int() 随即抛 ValueError，
#             被 do_POST 的兜底 except 变成 500 并把异常原文回显给客户端。

import json
import socket
import threading
import unittest
from unittest.mock import patch

import app
from test_security_boundaries import QuietServer, SessionHandler


class MalformedContentLengthTests(unittest.TestCase):
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

    def raw_post(self, length_bytes, body=b"{}"):
        """裸 socket 发送非 ASCII 的 Content-Length 原始字节。"""
        connection = socket.create_connection(self.server.server_address, timeout=3)
        try:
            request = (b"POST /api/service/restart HTTP/1.1\r\n"
                       b"Host: 127.0.0.1\r\n"
                       b"Content-Type: application/json\r\n"
                       b"X-Admin-Request: 1\r\n"
                       b"Connection: close\r\n"
                       b"Content-Length: " + length_bytes + b"\r\n\r\n" + body)
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

    def test_latin1_digit_byte_is_rejected_as_400_without_leaking_the_exception(self):
        with patch.object(app.Handler, "service_restart", return_value={"ok": True}) as action:
            raw = self.raw_post(b"\xb2")

        self.assertIn(b"HTTP/1.1 400 ", raw, "非 ASCII 数字的 Content-Length 应被拒为 400：%r" % raw[:400])
        self.assertNotIn(b"HTTP/1.1 500", raw)
        self.assertNotIn(b"Traceback", raw)
        self.assertNotIn(b"invalid literal for int", raw)
        # 400 响应必须是完整可解析的 JSON（请求体按合同被拒，动作不得执行）。
        _, _, body = raw.partition(b"\r\n\r\n")
        self.assertEqual(json.loads(body.decode("utf-8"))["ok"], False)
        action.assert_not_called()

    def test_ascii_control_length_still_parses(self):
        """对照：正常 ASCII 数字长度不受影响（解析成功，进入会话闸门 401）。"""
        raw = self.raw_post(b"2")
        self.assertIn(b"HTTP/1.1 401 ", raw)


if __name__ == "__main__":
    unittest.main()
