#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-24：通过隔离管理台复现改密时旧登录/续期越过会话失效边界，以及并发登录绕过失败预算。
import concurrent.futures
import http.client
import json
from http.cookies import SimpleCookie
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch

import app
from test_security_boundaries import QuietServer, RealSessionHandler


class CredentialGenerationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / 'credentials.json'
        self.patchers = [patch.object(app, 'CRED_PATH', str(self.path)),
                         patch.object(app, 'AUTH_DIR', self.directory.name)]
        for item in self.patchers:
            item.start()
            self.addCleanup(item.stop)
        self.original_password = 'fixture-original-password'
        self.rotated_password = 'fixture-rotated-password'
        self.document = {'version': 1, 'username': 'fixture-admin',
                         'password': app.hash_password(self.original_password),
                         'sessionKey': '12' * 32, 'updatedAt': 1, 'history': []}
        app._save_credentials(self.document)
        app._fails.clear()
        app._revoked.clear()
        self.server = QuietServer(('127.0.0.1', 0), RealSessionHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.close_server)

    def close_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)

    def request(self, method, path, body=None, token=None):
        headers = {'Content-Type': 'application/json', 'X-Admin-Request': '1'}
        if token:
            headers['Cookie'] = app.COOKIE_NAME + '=' + token
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=3)
        try:
            connection.request(method, path, json.dumps(body) if body is not None else None, headers)
            response = connection.getresponse()
            return response.status, dict(response.getheaders()), response.getheaders(), response.read()
        finally:
            connection.close()

    def rotate(self):
        updated = dict(self.document, password=app.hash_password(self.rotated_password),
                       sessionKey='34' * 32, updatedAt=2)
        with app._cred_lock:
            app._save_credentials(updated)

    def verify_then_rotate(self, original):
        def verify(password, stored):
            result = original(password, stored)
            if result:
                self.rotate()
            return result
        return verify

    def test_password_change_cannot_upgrade_an_old_password_login(self):
        original_verify = app.verify_password
        with patch.object(app, 'verify_password', side_effect=self.verify_then_rotate(original_verify)):
            status, _, _, _ = self.request('POST', '/api/auth/login',
                {'username': 'fixture-admin', 'password': self.original_password})
        self.assertEqual(status, 401, 'a stale password received a session for the new credentials')

    def test_concurrent_password_change_cannot_overwrite_new_credentials(self):
        token, _ = app.issue_session('fixture-admin')
        original_verify = app.verify_password
        with patch.object(app, 'verify_password', side_effect=self.verify_then_rotate(original_verify)):
            status, _, _, _ = self.request('POST', '/api/auth/password',
                {'username': 'fixture-admin', 'current': self.original_password,
                 'password': 'fixture-late-overwrite', 'confirm': 'fixture-late-overwrite'}, token)
        self.assertEqual(status, 401, 'a stale password change overwrote the newer credentials')
        self.assertTrue(app.verify_password(self.rotated_password, app.load_credentials()['password']))

    def test_sliding_renewal_does_not_survive_password_rotation(self):
        token, _ = app.issue_session('fixture-admin', ttl=120)
        original_read = app.read_session

        def read_then_rotate(*args, **kwargs):
            payload = original_read(*args, **kwargs)
            if payload:
                self.rotate()
            return payload

        with patch.object(app, 'read_session', side_effect=read_then_rotate):
            status, _, headers, _ = self.request('GET', '/api/session', token=token)
        self.assertEqual(status, 200)
        renewed = []
        for name, value in headers:
            if name.lower() == 'set-cookie':
                cookie = SimpleCookie(value)
                if app.COOKIE_NAME in cookie and cookie[app.COOKIE_NAME].value:
                    renewed.append(cookie[app.COOKIE_NAME].value)
        self.assertTrue(renewed)
        self.assertTrue(all(app.read_session(value) is None for value in renewed),
                        'renewal re-signed a stale session with the new signing key')

    def test_login_budget_counts_an_in_progress_attempt(self):
        entered, release = threading.Event(), threading.Event()
        attempts = []

        def delayed_verify(*_):
            attempts.append(1)
            if len(attempts) == 1:
                entered.set()
                release.wait(3)
            return False

        body = {'username': 'fixture-admin', 'password': 'fixture-wrong-password'}
        with patch.object(app, 'LOGIN_MAX_FAILS', 1), patch.object(app, 'verify_password', side_effect=delayed_verify):
            with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
                first = executor.submit(self.request, 'POST', '/api/auth/login', body)
                try:
                    self.assertTrue(entered.wait(2))
                    second_status, _, _, _ = self.request('POST', '/api/auth/login', body)
                finally:
                    release.set()
                self.assertEqual(first.result()[0], 401)
        self.assertEqual(second_status, 429, 'concurrent login exceeded the attempt budget')
        self.assertEqual(len(attempts), 1)


if __name__ == '__main__':
    unittest.main()
