#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-30：锁定面板与网关的 auth 文件契约——宽 glob（workbuddy*.json，与网关
#             AuthFileGlob 同口径）与扁平形 auth 文件（审查发现 19）。此前面板的正则要求
#             文件名带连字符，网关实际加载的无连字符账号在面板里不可见、也管不了。
# 2026-09-18：验证账号写入失败、启停冲突和快速重复删除时仍保留已有凭据与回收件。

import json
import itertools
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import app


class AuthFileListingTests(unittest.TestCase):
    """面板枚举 auth 文件的口径必须覆盖网关会加载的全部文件名。"""

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.auths = Path(self.directory.name) / "auths"
        self.auths.mkdir()
        p = patch.object(app, "AUTHS_DIR", str(self.auths))
        p.start()
        self.addCleanup(p.stop)

    def write(self, name, doc):
        (self.auths / name).write_text(json.dumps(doc), encoding="utf-8")

    @staticmethod
    def nested(uid):
        return {"auth": {"accessToken": "tok", "expiresAt": 1893456000, "realm": "cn",
                         "domain": "copilot.tencent.com"},
                "account": {"uid": uid, "nickname": "Nick " + uid[:4]}}

    @staticmethod
    def flat(uid):
        return {"accessToken": "tok", "expiresAt": 1893456000, "uid": uid,
                "nickname": "Flat " + uid[:4], "realm": "cn",
                "domain": "copilot.tencent.com"}

    def test_hyphenless_files_are_listed(self):
        # 回归（审查发现 19）：workbuddy_new.json / workbuddy.json 网关会加载，面板此前 HIDDEN。
        self.write("workbuddy-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json", self.nested("a" * 32))
        self.write("workbuddy_new.json", self.flat("b" * 32))
        self.write("workbuddy.json", self.flat("c" * 32))
        files = [e["file"] for e in app.list_auth_files()]
        self.assertEqual(files, ["workbuddy-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json",
                                 "workbuddy.json", "workbuddy_new.json"])

    def test_disabled_suffix_is_still_recognized(self):
        self.write("workbuddy_new.json.disabled", self.flat("b" * 32))
        items = app.list_auth_files()
        self.assertEqual(len(items), 1)
        self.assertTrue(items[0]["disabled"])
        self.assertEqual(items[0]["file"], "workbuddy_new.json.disabled")

    def test_non_auth_files_are_ignored(self):
        self.write("workbuddy-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json", self.nested("a" * 32))
        (self.auths / "notes.txt").write_text("x", encoding="utf-8")
        (self.auths / "other.json").write_text("{}", encoding="utf-8")
        self.assertEqual([e["file"] for e in app.list_auth_files()],
                         ["workbuddy-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json"])

    def test_uid_comes_from_content_not_filename(self):
        # 无连字符命名下文件名切不出 uid，必须从文件内容取（网关也按内容 uid 索引）。
        self.write("workbuddy_new.json", self.flat("d" * 32))
        self.assertEqual(app.list_auth_files()[0]["uid"], "d" * 32)

    def test_uid_falls_back_to_filename_when_content_unreadable(self):
        # 内容损坏时不能整条消失，回落到文件名中间段（去掉可能的连字符前缀），保持可启停/删除。
        (self.auths / "workbuddy-e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5.json").write_text("{broken", encoding="utf-8")
        self.assertEqual(app.list_auth_files()[0]["uid"], "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5")
        (self.auths / "workbuddy-f0f0f0f0.json").write_text("{broken", encoding="utf-8")
        self.assertEqual(app.find_entry("f0f0f0f0")["file"], "workbuddy-f0f0f0f0.json")

    def test_flat_form_summary_reads_top_level_fields(self):
        # 扁平形此前 auth_summary 读不到 realm/domain/expiresAt，账号卡片显示为空。
        self.write("workbuddy_new.json", self.flat("f" * 32))
        s = app.auth_summary(app.list_auth_files()[0])
        self.assertEqual(s["uid"], "f" * 32)
        self.assertEqual(s["realm"], "cn")
        self.assertEqual(s["domain"], "copilot.tencent.com")
        self.assertEqual(s["expiresAt"], 1893456000)
        self.assertEqual(s["nickname"], "Flat ffff")

    def test_find_entry_reaches_hyphenless_account(self):
        # 面板的启停/删除都经 find_entry；它此前只遍历窄 glob 的结果，够不到这类账号。
        self.write("workbuddy_new.json", self.flat("b" * 32))
        entry = app.find_entry("b" * 32)
        self.assertIsNotNone(entry, "hyphen-less account is invisible to account_toggle/account_delete")
        self.assertEqual(entry["file"], "workbuddy_new.json")


class AccountStorageTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.auths = self.root / "auths"
        self.auths.mkdir()
        self.trash = self.root / "trash"
        self.uid = "a" * 32
        self.active = self.auths / ("workbuddy-" + self.uid + ".json")
        self.disabled = self.active.with_name(self.active.name + ".disabled")
        self.handler = app.Handler.__new__(app.Handler)
        for name, value in [("AUTHS_DIR", str(self.auths)), ("TRASH_DIR", str(self.trash))]:
            p = patch.object(app, name, value); p.start(); self.addCleanup(p.stop)

    def entry(self, path, disabled=False):
        return {"uid": self.uid, "path": str(path), "file": path.name, "disabled": disabled}

    def test_failed_new_login_keeps_disabled_credential(self):
        self.disabled.write_text("previous credential", encoding="utf-8")
        with patch.object(app.os, "replace", side_effect=OSError("injected persistence failure")), patch.object(app, "chown_app"):
            with self.assertRaises(OSError):
                app.write_auth_file({"uid": self.uid, "access_token": "fixture-only-token", "expires_in": 3600})
        self.assertTrue(self.disabled.exists(), "failed save deleted the previous disabled credential")
        self.assertEqual(self.disabled.read_text(encoding="utf-8"), "previous credential")

    def test_toggle_conflict_does_not_delete_other_credential(self):
        self.active.write_text("active credential", encoding="utf-8")
        self.disabled.write_text("different disabled credential", encoding="utf-8")
        with patch.object(app, "find_entry", return_value=self.entry(self.active)), \
                patch.object(app, "chown_app"), patch.object(app, "restart_container", return_value=(True, "ok", 0)) as restart:
            result = self.handler.account_toggle({"uid": self.uid, "disabled": True})
        self.assertFalse(result["ok"])
        self.assertEqual(self.active.read_text(encoding="utf-8"), "active credential")
        self.assertEqual(self.disabled.read_text(encoding="utf-8"), "different disabled credential")
        restart.assert_not_called()

    def test_same_second_deletes_keep_both_recovery_files(self):
        with patch.object(app, "find_entry", side_effect=lambda uid: self.entry(self.active)), \
                patch.object(app.time, "strftime", return_value="20260918-120000"), \
                patch.object(app, "restart_container", return_value=(True, "ok", 0)):
            for value in ("first credential", "second credential"):
                self.active.write_text(value, encoding="utf-8")
                self.assertTrue(self.handler.account_delete({"uid": self.uid})["ok"])
        self.assertEqual(sorted(p.read_text(encoding="utf-8") for p in self.trash.iterdir()),
                         ["first credential", "second credential"])

    def test_restart_does_not_report_unhealthy_service_as_success(self):
        for health in (None, {"error": {"message": "unavailable"}}):
            with self.subTest(health=health), patch.object(app, "docker", return_value=(0, "container", "")), \
                    patch.object(app, "gateway_get", return_value=health), patch.object(app.time, "sleep"), \
                    patch.object(app.time, "monotonic", side_effect=itertools.count()):
                ok, _, _ = app.restart_container()
                self.assertFalse(ok)

    def test_restart_accepts_a_real_healthy_service_response(self):
        with patch.object(app, "docker", return_value=(0, "container", "")), \
                patch.object(app, "gateway_get", return_value={"service": "workbuddy2api", "healthy": 0}), \
                patch.object(app.time, "sleep"):
            ok, _, _ = app.restart_container()
            self.assertTrue(ok)


if __name__ == "__main__":
    unittest.main()
