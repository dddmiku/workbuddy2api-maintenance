#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-17：锁定表格数值列对齐：表头用 th.r、数据用 td.num，两边都要右对齐，
#             否则会出现「表头在右、数字在左」的错位（用户实测反馈）。
# 2026-09-17：锁定启动顺序：拼接脚本里只能有一处 init()，且必须等全部段执行完。
# 2026-09-17：锁定首屏健壮性：直接打开 /#system 时数据还没到，渲染不能抛异常。

import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
INDEX = os.path.join(HERE, "index.html")


def read_source(name):
    with open(os.path.join(HERE, "src", name), "r", encoding="utf-8") as fh:
        return fh.read()


def strip_css_comments(css):
    """去掉 /* … */ 注释。断言选择器时必须先剥注释：修复说明里会引用旧写法，
    直接搜字符串会把「注释提到旧规则」误判成「旧规则还在」。"""
    out = []
    i = 0
    while True:
        start = css.find("/*", i)
        if start < 0:
            out.append(css[i:])
            break
        out.append(css[i:start])
        end = css.find("*/", start + 2)
        if end < 0:
            break
        i = end + 2
    return "".join(out)


class TableAlignmentTests(unittest.TestCase):
    def test_log_table_right_aligns_header_and_cells(self):
        css = read_source("logs.css")
        self.assertIn(".log-table td.num", css)
        self.assertIn(".log-table th.r", css)
        rule = [line for line in css.splitlines() if ".log-table td.num" in line][0]
        self.assertIn("text-align:right", rule)

    def test_usage_table_right_aligns_header_and_cells(self):
        css = read_source("usage.css")
        self.assertIn(".usage-table td.num", css)
        self.assertIn(".usage-table th.r", css)
        rule = [line for line in css.splitlines() if ".usage-table td.num" in line][0]
        self.assertIn("text-align:right", rule)

    def test_numeric_headers_use_right_class(self):
        body = read_source("body.html")
        # 日志页的 token 列：输入 / 缓存命中 / 输出（tok 改名为输出，语义不变）。
        for header in ("TTFB", "输入", "缓存命中", "输出", "tok/s", "total"):
            with self.subTest(header=header):
                self.assertIn('<th class="r">%s</th>' % header, body)
        for header in ("请求", "输入 tokens", "缓存命中", "输出 tokens", "合计 tokens"):
            with self.subTest(header=header):
                self.assertIn('<th class="r">%s</th>' % header, body)

    def test_numeric_cells_use_num_class(self):
        with open(os.path.join(HERE, "app.js"), "r", encoding="utf-8") as fh:
            logs_js = fh.read()
        with open(os.path.join(HERE, "usage.js"), "r", encoding="utf-8") as fh:
            usage_js = fh.read()
        self.assertIn('<td class="mono num">', logs_js)
        # 用量表数字列带 data-l（窄屏折叠成卡片时显示行内标签），class 仍然是 num。
        self.assertIn('<td class="num"', usage_js)

    def test_built_index_contains_alignment_rules(self):
        # 生成的 index.html 才是真正被浏览器加载的文件：规则必须真的拼进去了。
        with open(INDEX, "r", encoding="utf-8") as fh:
            html = fh.read()
        self.assertIn(".log-table td.num,.log-table th.r{text-align:right}", html)
        self.assertIn(".usage-table td.num,.usage-table th.r{text-align:right}", html)
        self.assertIn('<th class="r">TTFB</th>', html)
        self.assertIn('<td class="mono num">', html)


class BootOrderTests(unittest.TestCase):
    """拼接脚本的启动顺序：keys.js 曾自己调 init()，在 usage.js 之前执行，
    导致从 #usage 进入时 US 未定义、页面永远停在加载态。"""

    def test_only_app_js_starts_the_app(self):
        for name in ("keys.js", "usage.js"):
            with self.subTest(name=name):
                with open(os.path.join(HERE, name), "r", encoding="utf-8") as fh:
                    lines = [line.strip() for line in fh]
                calls = [line for line in lines if line == "init();"]
                self.assertEqual(calls, [], "%s 不应自己调用 init()" % name)

    def test_app_js_defers_start_until_script_finishes(self):
        with open(os.path.join(HERE, "app.js"), "r", encoding="utf-8") as fh:
            js = fh.read()
        self.assertIn("document.addEventListener('DOMContentLoaded', init)", js)
        # 末尾必须是延迟启动，不能是裸 init()
        self.assertFalse(js.rstrip().endswith("init();"),
                         "app.js 末尾不能直接 init()：那时后续拼接段还没执行")

    def test_built_index_has_single_deferred_start(self):
        with open(INDEX, "r", encoding="utf-8") as fh:
            html = fh.read()
        body = html.split("<script>", 1)[-1]
        self.assertEqual(body.count("\ninit();"), 0,
                         "生成的页面里不应再有裸 init() 调用")
        self.assertIn("DOMContentLoaded", body)


class FirstPaintTests(unittest.TestCase):
    """首屏渲染不能依赖"数据已经拉回来"：面板允许带 #view 直接打开或刷新。"""

    def setUp(self):
        with open(os.path.join(HERE, "app.js"), "r", encoding="utf-8") as fh:
            self.js = fh.read()

    def test_render_system_tolerates_missing_state(self):
        start = self.js.index("function renderSystem(){")
        body = self.js[start:self.js.index("\n}", start)]
        self.assertIn("if (!d) return", body,
                      "renderSystem 必须先判空：否则直接打开 #system 会抛异常并中断 go()")

    def test_system_view_loads_update_card_before_rendering(self):
        start = self.js.index("function go(v){")
        body = self.js[start:self.js.index("\n}", start)]
        self.assertLess(body.index("loadUpdate()"), body.index("renderSystem()"),
                        "更新卡片不依赖 /api/state，必须在 renderSystem 之前触发")

    def test_built_index_keeps_system_guards(self):
        with open(INDEX, "r", encoding="utf-8") as fh:
            html = fh.read()
        self.assertIn("function renderSystem(){", html)
        self.assertIn("if (!d) return", html)
        self.assertIn("loadUpdate()", html)
        self.assertIn("#updRows", html)


class TableBorderCollapseTests(unittest.TestCase):
    """折叠边框的前提：参与合并的单元格必须是 table-cell。

    `border-collapse:collapse` 只合并**真正的 table-cell** 的边框。一旦某个 td
    被 display:flex/grid/block 化，它就被排除在合并之外——border-bottom 计算样式
    仍报 1px，却完全不绘制。2026-10-08 实测症状：账号表在操作列起点出现硬断点，
    同一行左侧有线、右侧无线（行数越多越明显，看起来像"线没对齐"）。
    """

    def test_action_cells_keep_table_cell_display(self):
        css = strip_css_comments(read_source("css_c.css"))
        # 操作列的 flex 必须挂在 td 内层容器上，不能直接写 `.tbl .acts{display:flex}`。
        self.assertNotIn(".tbl .acts{display:flex", css,
                         "td 自己 flex 化会让折叠边框不再绘制它；flex 应放在内层 div 上")
        self.assertIn(".tbl .row-acts{display:flex", css)

    def test_account_rows_use_inner_container_for_actions(self):
        with open(os.path.join(HERE, "app.js"), "r", encoding="utf-8") as fh:
            js = fh.read()
        self.assertIn('<td class="r" data-l="操作"><div class="row-acts">', js,
                      "账号表操作列必须保持 td 为 table-cell，flex 交给内层 div")
        self.assertNotIn('<td class="acts">', js)

    def test_narrow_screen_still_stacks_action_buttons(self):
        css = strip_css_comments(read_source("css_c.css"))
        # 窄屏把 td 变成 flex 卡片行，此时内层容器要让位，否则按钮被挤成一列。
        self.assertIn(".tbl .row-acts{display:contents}", css)
        # 横向滚动档把 td 还原成 table-cell，内层 flex 随之恢复。
        headers = strip_css_comments(read_source("table_headers.css"))
        self.assertIn(".table-scroll .tbl .row-acts{display:flex", headers)


if __name__ == "__main__":
    unittest.main()
