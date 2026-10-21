#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-19：新增。锁定动态任务推断的两条实测缺陷：
#             ① 专家类任务被「对话」抢先命中，全被推成 chat；
#             ② 专家关键词把「专家」后缀带进搜索词，市场搜不到。
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import task_runner as tr  # noqa: E402


def spec_for(title, desc, jump="", target=1):
    t = {"title": title, "task_desc": desc, "jump_url": jump,
         "progress": {"target": target}}
    return tr.build_dynamic_spec(t)


class InferKindTests(unittest.TestCase):
    """用 2026-10-19 服务端返回的真实 16 个任务做回归。"""

    CASES = [
        # (title, desc, jump_url, 期望 kind)
        ("体验「设计创意模式」", "在「设计创意」模式下成功创建1个画布。",
         "workbuddy://chat", "canvas"),
        ("探索优秀灵感", "体验1款「灵感」优秀案例。",
         "workbuddy://playbook", "playbook"),
        ("体验「资料库」", "前往「资料库」，读完《WorkBuddy 资料库介绍》这篇文档",
         "workbuddy://library", "library"),
        ("体验「腾讯轻量云」专家",
         "召唤专家，在对话框上方授权连接器后，对话一次，免费领取一个月轻量应用服务器",
         "workbuddy://expert", "expert_task"),
        ("体验「和平精英」主题",
         "升级到电脑端 5.5.3 及以上版本，点击「菜单-外观」，使用「和平精英」主题。",
         "workbuddy://settings/appearance", "skin"),
        ("体验 WorkBuddy 「发现应用」",
         "升级到电脑端5.5.3或以上版本，点击左上角「发现应用」进入任意一个 Buddy 应用。",
         "workbuddy://home?openBuddySwitcher=true", "buddy5"),
        ("体验「无畏契约拼豆专家」",
         "即日起至11月15日，在专家页搜索并召唤「无畏契约拼豆专家」，完成一次对话。",
         "workbuddy://expert", "expert_task"),
        ("召唤 3 次专家团", "切换至「专家团」，召唤并使用 3 个不同的专家团队",
         "workbuddy://experts", "team"),
        ("领取一只 Buddy", "新建 1 个任务并发起对话。",
         "workbuddy://chat", "chat"),
        ("和 AI 聊天 5 次", "和AI对话累计 5 次。",
         "workbuddy://chat", "chat"),
        ("尝鲜热门技能", "安装并使用 1 个技能。",
         "workbuddy://skills", "skill"),
        ("召唤 5 次专家", "召唤 5 位不同的专家并发起对话。",
         "workbuddy://expert", "expert_task"),
        ("使用 5 个模板", "「新建任务」并使用 5 个不同的模板并发起对话。",
         "workbuddy://templates", "template"),
        ("设置自动化任务", "设置 1 个自动化任务。",
         "workbuddy://automation", "automation"),
    ]

    def test_real_tasks_infer_expected_kind(self):
        # 回归（2026-10-19）：KIND_RULES 里「对话|聊天」排在「专家」之前，而 16 个
        # 任务里 13 个描述含「对话」，Expert_lighthouse / WorkBuddy_Valorant /
        # expert_5 全被推成 chat —— 新专家活动会因此上报错事件，progress 恒 0。
        for title, desc, jump, want in self.CASES:
            with self.subTest(title=title):
                spec, _ = spec_for(title, desc, jump)
                self.assertIsNotNone(spec, "应能推断出 kind")
                self.assertEqual(spec["kind"], want)

    def test_jump_url_does_not_override_specific_text(self):
        # create_canvas 的 jump_url 是 workbuddy://chat，但它是画布任务；
        # Expert_team_use_3 指向 //experts，但它是专家团不是单个专家。
        # jump_url 只能修正「文本判不出」或「只落到宽泛 chat」的情况。
        spec, _ = spec_for("体验「设计创意模式」", "创建1个画布。", "workbuddy://chat")
        self.assertEqual(spec["kind"], "canvas")
        spec, _ = spec_for("召唤 3 次专家团", "召唤并使用 3 个不同的专家团队",
                           "workbuddy://experts")
        self.assertEqual(spec["kind"], "team")

    def test_jump_url_rescues_when_text_is_generic(self):
        # 文本只有「对话」但 jump_url 明确指向专家页 —— 应修正为 expert_task
        spec, _ = spec_for("体验新专家", "完成一次对话。", "workbuddy://expert")
        self.assertEqual(spec["kind"], "expert_task")

    def test_unforgeable_task_is_skipped(self):
        spec, note = spec_for("关注「腾讯 WorkBuddy」官方公众号",
                              "关注满 24 小时，即可领取奖励。",
                              "https://www.workbuddy.cn/events/follow-guide/")
        self.assertIsNone(spec)
        self.assertIn("公众号", note)

    def test_target_comes_from_server_progress(self):
        # 动态任务不知道要几次：硬写 1 会让「召唤 5 次专家」只点亮一次。
        spec, _ = spec_for("召唤 5 次专家", "召唤 5 位不同的专家并发起对话。",
                           "workbuddy://expert", target=5)
        self.assertEqual(spec["target"], 5)


class ExpertKeywordTests(unittest.TestCase):
    def test_strips_expert_suffix(self):
        # 市场按名字搜，带「专家」后缀搜不到（实测 keyword=无畏契约拼豆专家 为空）
        self.assertEqual(
            tr.extract_expert_keyword("体验「无畏契约拼豆专家」",
                                      "搜索并召唤「无畏契约拼豆专家」，完成一次对话。"),
            ["无畏契约拼豆"])

    def test_extracts_bracketed_name(self):
        self.assertEqual(
            tr.extract_expert_keyword("体验「腾讯轻量云」专家", "召唤专家，对话一次"),
            ["腾讯轻量云"])

    def test_does_not_swallow_sentence_without_quotes(self):
        # 无引号的「召唤 X」正则曾跨标点吞整句
        # （`召唤 3 次专家团，切换至「专家团」` -> `3 次专家团 切换至`），
        # 拿去搜市场必然为空。抽不出就返回空列表。
        self.assertEqual(
            tr.extract_expert_keyword("召唤 3 次专家团",
                                      "切换至「专家团」，召唤并使用 3 个不同的专家团队"),
            [])
        self.assertEqual(tr.extract_expert_keyword("召唤 5 次专家",
                                                   "召唤 5 位不同的专家并发起对话。"), [])


if __name__ == "__main__":
    unittest.main()
