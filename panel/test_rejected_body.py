#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-24：锁定提前拒绝的HTTP响应可读性，以及丢弃迟到请求体的100ms绝对截止、64KiB预算和无效分帧边界。

from contextlib import ExitStack
from email.message import Message
import http.client
import threading
import unittest
from unittest.mock import Mock, patch

import app
from test_security_boundaries import QuietServer, SessionHandler


class RejectionGateHandler(SessionHandler):
    def _json(self, code, payload, extra=None):
        result = super()._json(code, payload, extra)
        if code in (401, 403):
            self.server.reply_sent.set()
            if not self.server.body_sent.wait(timeout=3):
                raise RuntimeError("fixture did not release the delayed request body")
        return result


class RejectionGateServer(QuietServer):
    def __init__(self):
        self.reply_sent = threading.Event()
        self.body_sent = threading.Event()
        self.request_closed = threading.Event()
        self.handler_errors = []
        super().__init__(("127.0.0.1", 0), RejectionGateHandler)

    def shutdown_request(self, request):
        super().shutdown_request(request)
        self.request_closed.set()

    def handle_error(self, request, client_address):
        self.handler_errors.append(client_address)


class RejectedBodyHTTPTests(unittest.TestCase):
    def setUp(self):
        self.server = RejectionGateServer()
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.body_sent.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)
        self.assertFalse(self.thread.is_alive())

    def rejected_request_with_late_body(self, path, expected, extra_headers):
        headers = {"Content-Type": "application/json", "Content-Length": "2",
                   "Cookie": "fixture-session", "X-Admin-Request": "1"}
        headers.update(extra_headers)
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=3)
        try:
            with ExitStack() as patches:
                actions = [patches.enter_context(patch.object(app.Handler, name))
                           for name in ("login_start", "keys_request")]
                connection.putrequest("POST", path)
                for name, value in headers.items():
                    if value is not None:
                        connection.putheader(name, value)
                connection.endheaders()
                self.assertTrue(self.server.reply_sent.wait(timeout=3), "server did not send the rejection")
                connection.send(b"{}")
                self.server.body_sent.set()
                self.assertTrue(self.server.request_closed.wait(timeout=3), "rejected connection was not bounded")
                response = connection.getresponse()
                self.assertEqual(response.status, expected)
                self.assertEqual(response.getheader("Connection"), "close")
                self.assertTrue(response.read())
                for action in actions:
                    action.assert_not_called()
                self.assertEqual(self.server.handler_errors, [])
        finally:
            self.server.body_sent.set()
            connection.close()

    def test_invalid_origin_still_returns_403_with_a_late_body(self):
        self.rejected_request_with_late_body("/api/login/start", 403, {"Origin": "https://untrusted.invalid"})

    def test_missing_management_header_still_returns_403_with_a_late_body(self):
        self.rejected_request_with_late_body("/api/login/start", 403, {"X-Admin-Request": ""})

    def test_unauthenticated_key_request_still_returns_401_with_a_late_body(self):
        self.rejected_request_with_late_body("/api/keys/copy", 401, {"Cookie": None})

    def test_cross_origin_key_request_still_returns_403_with_a_late_body(self):
        self.rejected_request_with_late_body("/api/keys/copy", 403, {"Origin": "https://untrusted.invalid"})


class DrainConnection:
    def __init__(self):
        self.timeout = None
        self.timeouts = []

    def gettimeout(self):
        return self.timeout

    def settimeout(self, value):
        self.timeout = value
        self.timeouts.append(value)


class RejectedBodyBudgetTests(unittest.TestCase):
    def handler(self, length="2", headers=None):
        handler = object.__new__(app.Handler)
        handler.headers = Message()
        if length is not None:
            handler.headers.add_header("Content-Length", length)
        for name, value in headers or []:
            handler.headers.add_header(name, value)
        handler.close_connection = False
        handler.connection = DrainConnection()
        handler.rfile = Mock()
        handler._json = Mock()
        return handler

    def test_rejection_is_sent_before_discarding_the_body(self):
        handler = self.handler()
        order = []
        handler._json.side_effect = lambda *args, **kwargs: order.append("rejected")

        def read(size):
            order.append("discarded")
            self.assertEqual(size, 2)
            return b"{}"

        handler.rfile.read1.side_effect = read
        app.Handler._reject_unread_body(handler, 403, {"ok": False})
        self.assertEqual(order, ["rejected", "discarded"])
        self.assertTrue(handler.close_connection)
        handler._json.assert_called_once()
        self.assertEqual(handler._json.call_args.args[0], 403)

    def test_absolute_deadline_is_not_extended_by_slow_trickle(self):
        handler = self.handler("1000")
        clock, reads = [1000.0], []

        def read(size):
            handler._json.assert_called_once()
            remaining = handler.connection.timeout
            self.assertIsNotNone(remaining)
            self.assertGreater(remaining, 0)
            self.assertLessEqual(remaining, 0.1000001)
            reads.append(size)
            self.assertLessEqual(len(reads), 7, "per-read timeout kept extending the total deadline")
            if remaining < 0.015:
                clock[0] += remaining
                raise TimeoutError("fixture budget reached")
            clock[0] += 0.015
            return b"x"

        handler.rfile.read1.side_effect = read
        with patch.object(app.time, "monotonic", side_effect=lambda: clock[0]):
            app.Handler._reject_unread_body(handler, 403, {"ok": False})
        self.assertGreater(len(reads), 0)
        self.assertLessEqual(clock[0] - 1000.0, 0.1000001)
        self.assertTrue(handler.close_connection)

    def test_discard_never_exceeds_64kib_even_with_a_huge_declared_length(self):
        handler = self.handler("1000000000")
        consumed = [0]

        def read(size):
            self.assertGreater(size, 0)
            self.assertLessEqual(size, 4096)
            consumed[0] += size
            self.assertLessEqual(consumed[0], 65536)
            return b"x" * size

        handler.rfile.read1.side_effect = read
        with patch.object(app.time, "monotonic", return_value=1000.0):
            app.Handler._reject_unread_body(handler, 403, {"ok": False})
        self.assertEqual(consumed[0], 65536)
        self.assertTrue(handler.close_connection)

    def test_invalid_or_ambiguous_lengths_do_not_wait_for_a_body(self):
        cases = [(None, []), ("invalid", []), ("-1", []), ("+2", []), ("2.0", []),
                 ("0", []), ("9" * 5000, []), ("2", [("Content-Length", "2")]),
                 ("2", [("Content-Length", "3")]), ("2", [("Transfer-Encoding", "chunked")])]
        for length, headers in cases:
            with self.subTest(length=length[:20] if length else None, headers=headers):
                handler = self.handler(length, headers)
                app.Handler._reject_unread_body(handler, 403, {"ok": False})
                handler._json.assert_called_once()
                handler.rfile.read1.assert_not_called()
                self.assertTrue(handler.close_connection)

    def test_peer_disconnect_or_read_timeout_does_not_replace_rejection(self):
        for error in (ConnectionResetError("closed"), TimeoutError("late"), ValueError("closed stream")):
            with self.subTest(error=type(error).__name__):
                handler = self.handler()
                handler.rfile.read1.side_effect = error
                app.Handler._reject_unread_body(handler, 403, {"ok": False})
                handler._json.assert_called_once()
                self.assertEqual(handler._json.call_args.args[0], 403)
                self.assertTrue(handler.close_connection)


if __name__ == "__main__":
    unittest.main()
