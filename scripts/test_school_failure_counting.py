#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-02：锁定 school_open_day_2026 两条失败计数契约——抽奖返回错误码必须计入
#             fail（体检发现 18）；report 上报抛错必须计入 fail 而非 pending（体检发现 19）。
import os
import sys
import unittest
from unittest.mock import patch

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import school_open_day_2026 as sch  # noqa: E402

UID = "abcdef1234567890abcdef1234567890"


class _Opts:
    yes = True
    gap = 1.0


def _stats():
    return {"accounts": 0, "ok": 0, "already": 0, "skip": 0, "pending": 0, "fail": 0}


class LotteryFailureTests(unittest.TestCase):
    def test_draw_error_code_counts_fail(self):
        # 回归（2026-10-02 第二轮体检发现 18）：draw 返回非 200/非零业务码（非 40900）时
        # 此前只 break、不记 fail，脚本按契约应退出非零让调度器知道抽奖停了。
        stats = _stats()
        with patch.object(sch, "fetch_lottery_config",
                          return_value=({"in_period": True}, {"balance": 2, "total_earned": 2})), \
             patch.object(sch, "post_lottery_draw", return_value=(500, {"code": 500, "message": "boom"})), \
             patch.object(sch.time, "sleep", lambda *a, **k: None):
            sch.lottery_account({"uid": UID, "token": "t"}, _Opts(), stats)
        self.assertEqual(stats["fail"], 1)

    def test_draw_40900_no_chance_is_not_failure(self):
        # 40900「没机会」是合法边界，不算失败。
        stats = _stats()
        with patch.object(sch, "fetch_lottery_config",
                          return_value=({"in_period": True}, {"balance": 1, "total_earned": 1})), \
             patch.object(sch, "post_lottery_draw", return_value=(409, {"code": 40900, "message": "no chance"})), \
             patch.object(sch.time, "sleep", lambda *a, **k: None):
            sch.lottery_account({"uid": UID, "token": "t"}, _Opts(), stats)
        self.assertEqual(stats["fail"], 0)


class ReportFailureTests(unittest.TestCase):
    def test_report_transient_error_counts_fail(self):
        # 回归（2026-10-02 第二轮体检发现 19）：report_events 重试耗尽后抛 TransientError，
        # 此前 break 后回读无变化被记 pending（视为成功），退出码 0；应记 fail。
        stats = _stats()
        task = {"task_code": "chat_3_times", "status": "in_progress",
                "progress": 1, "target_count": 3, "title": "x"}
        with patch.object(sch, "fetch_school_tasks", return_value=([task], True)), \
             patch.object(sch, "report_events", side_effect=sch.TransientError("http 500")), \
             patch.object(sch, "fetch_lottery_config",
                          return_value=({"in_period": True}, {"balance": 0})), \
             patch.object(sch.time, "sleep", lambda *a, **k: None):
            sch.run_account({"uid": UID, "token": "t", "nick": "n"}, _Opts(), stats)
        self.assertEqual(stats["fail"], 1)


if __name__ == "__main__":
    unittest.main()
