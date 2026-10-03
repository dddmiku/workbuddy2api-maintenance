#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-02：锁定 task_runner 两条批量健壮性契约——单账号上游异常计入 fail 且不中断
#             整轮 ALL（体检发现 17）；template_5 事件的 name 必须是字符串（体检发现 21）。
import contextlib
import io
import os
import sys
import unittest
from unittest.mock import patch

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import task_runner as tr  # noqa: E402

UID = "abcdef1234567890abcdef1234567890"
UID2 = "ffffffff" + UID[8:]


def exit_code(main):
    with contextlib.redirect_stdout(io.StringIO()):
        try:
            return main() or 0
        except SystemExit as exc:
            return exc.code or 0


class BatchResilienceTests(unittest.TestCase):
    def test_process_account_exception_counts_fail_and_continues(self):
        # 回归（2026-10-02 第二轮体检发现 17）：process_account 内部 task_status 抛
        # RuntimeError 时，main 此前只 except SystemExit，异常逃逸导致整轮 ALL 中断、
        # 后续账号全部丢弃且不打印汇总。修复后应计入 fail 并继续下一个账号。
        calls = []

        def dispatch(auth, opts, stats):
            calls.append(auth["uid"])
            if len(calls) == 1:
                stats["fail"] += 1
                raise RuntimeError("list_tasks http=500")
            stats["ok"] += 1

        accounts = [{"uid": UID, "realm": "cn", "token": "t"},
                    {"uid": UID2, "realm": "cn", "token": "t"}]
        with patch.object(sys, "argv", ["task_runner.py", "ALL", "--yes"]), \
             patch.object(tr.tc, "all_auth_files", return_value=["a.json", "b.json"]), \
             patch.object(tr.tc, "load_auth", side_effect=accounts), \
             patch.object(tr, "process_account", side_effect=dispatch):
            code = exit_code(tr.main)
        self.assertEqual(calls, [UID, UID2])  # 第二个账号仍被处理
        self.assertNotEqual(code, 0)

    def test_template_event_name_is_string(self):
        # 回归（2026-10-02 第二轮体检发现 21）：template 事件此前把整个 meta dict 塞进
        # name（"name": m or ""），上游 schema 期望字符串；其他模板都用 m.get("name")。
        auth = {"uid": UID, "nick": "n", "token": "t"}
        ev = tr.build_event(auth, "template", "2", "视频生成", 0)
        self.assertIsInstance(ev["name"], str)
        self.assertEqual(ev["name"], "视频生成")


if __name__ == "__main__":
    unittest.main()
