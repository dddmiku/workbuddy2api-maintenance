# ═══ 更新日志 ═══
# 2026-09-26：锁定登录限流的 IPv6 /64 分桶、全局节流与管理入口白名单。
import importlib
import ipaddress
import os
import unittest

import app


class FakeHandler:
    def __init__(self, peer="203.0.113.9"):
        self.client_address = (peer, 12345)
        self.headers = {}


class BucketKeyTests(unittest.TestCase):
    def test_ipv6_addresses_share_a_64_bucket(self):
        first = app._bucket_key("2001:db8:1:2:3:4:5:6")
        second = app._bucket_key("2001:db8:1:2:ffff:ffff:ffff:ffff")
        self.assertEqual(first, second)
        self.assertEqual(first, str(ipaddress.ip_network("2001:db8:1:2::/64")))
        self.assertNotEqual(first, app._bucket_key("2001:db8:1:3::1"))

    def test_ipv4_and_garbage_keys(self):
        self.assertEqual(app._bucket_key("198.51.100.7"), "198.51.100.7")
        self.assertEqual(app._bucket_key("not-an-address"), "not-an-address")

    def test_rotating_addresses_hit_one_bucket(self):
        app._fails.clear()
        for index in range(app.LOGIN_MAX_FAILS):
            app.login_failed("2001:db8:9:9::%x" % index)
        blocked, hits = app.login_blocked("2001:db8:9:9::dead")
        self.assertTrue(blocked)
        self.assertEqual(hits, app.LOGIN_MAX_FAILS)
        app._fails.clear()


class GlobalThrottleTests(unittest.TestCase):
    def setUp(self):
        app._fails.clear()
        app._global_fails.clear()

    def tearDown(self):
        app._fails.clear()
        app._global_fails.clear()

    def test_distributed_failures_are_throttled(self):
        self.assertEqual(app.login_global_retry_after(), 0)
        for index in range(app.LOGIN_GLOBAL_MAX_FAILS):
            app.login_failed("198.51.100.%d" % (index % 254 + 1))
        self.assertGreater(app.login_global_retry_after(), 0)
        # 成功登录只清理自己的桶，全局节流按窗口自然过期。
        app._global_fails.clear()
        self.assertEqual(app.login_global_retry_after(), 0)

    def test_below_the_cap_stays_open(self):
        for _ in range(app.LOGIN_GLOBAL_MAX_FAILS - 1):
            app.login_failed("198.51.100.5")
        self.assertEqual(app.login_global_retry_after(), 0)


class AllowlistTests(unittest.TestCase):
    def _with_cidrs(self, value):
        os.environ["WB2API_ADMIN_ALLOW_CIDRS"] = value
        importlib.reload(app)
        self.addCleanup(self._restore)

    def _restore(self):
        os.environ.pop("WB2API_ADMIN_ALLOW_CIDRS", None)
        importlib.reload(app)

    def test_unset_allows_everyone(self):
        self._with_cidrs("")
        self.assertTrue(app.admin_client_allowed(FakeHandler("203.0.113.9")))

    def test_configured_allows_only_listed_networks_and_loopback(self):
        self._with_cidrs("203.0.113.0/24")
        self.assertTrue(app.admin_client_allowed(FakeHandler("203.0.113.50")))
        self.assertTrue(app.admin_client_allowed(FakeHandler("127.0.0.1")))
        self.assertFalse(app.admin_client_allowed(FakeHandler("198.51.100.7")))

    def test_invalid_entries_are_ignored(self):
        self._with_cidrs("bogus, 203.0.113.0/24")
        self.assertTrue(app.admin_client_allowed(FakeHandler("203.0.113.50")))
        self.assertFalse(app.admin_client_allowed(FakeHandler("198.51.100.7")))


if __name__ == "__main__":
    unittest.main()
