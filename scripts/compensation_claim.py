#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""补偿积分领取（一次性任务）。

背景（2026-10-16）：WorkBuddy 在「套餐与积分」页挂了一条限时补偿
（DeepSeek 模型服务异常额外补偿 500 积分），页面上的「立即领取」按钮
在 10/22 前可点。本脚本把它做成一次性任务：对所有符合条件的账号领取。

接口（从前端 bundle index-D7kQlGMt.js 挖出并实测确认，2026-10-16）：

    POST /billing/meter/compensation-status   查状态
    POST /billing/meter/claim-compensation    领取

两者都是 POST、无参数。响应：

    {"code":0,"msg":"OK","data":{
        "claimed": false,            # 是否已领
        "active": true,              # 活动是否进行中
        "credit_num": 500,           # 补偿积分数
        "validity_period": 1,
        "end_time": "2026-10-22 23:59:59"
    }}

领取成功的判据是响应 `code == 0`（与前端一致：`(await vp()).data?.code===0`
才提示成功并刷新积分）。

═══ 为什么是「一次性任务」而不是排程任务 ═══

补偿是限时活动（end_time 一过就没了），且每个账号**只能领一次**
（claimed=true 后重复调返回非 0 code）。做成排程任务会让它在活动结束后
每天空跑，还会在日志里刷「已领取」的噪音。面板上以手动触发的方式提供。

═══ 幂等性 ═══

先查 status：claimed=true 直接跳过（不调用领取接口）。这既省一次请求，
也避免依赖上游对重复领取的处置方式——那种处置（报错还是静默成功）
上游随时可能改。

用法：
    python3 compensation_claim.py                 # 所有账号
    python3 compensation_claim.py --uid <uid>     # 指定账号
    python3 compensation_claim.py --dry-run       # 只查状态，不领取
"""
import argparse
import sys

sys.path.insert(0, __file__.rsplit("/", 1)[0])

from task_common import (all_auth_files, auth_is_global, do_post, load_auth,
                         billing_base)

PATH_STATUS = "/billing/meter/compensation-status"
PATH_CLAIM = "/billing/meter/claim-compensation"

# 补偿活动只在 CN 站（workbuddy.cn / codebuddy.cn）挂着；global 与 trae 账号打这个
# 端点会拿到 APISIX 的 401 授权页——那是**预期行为**（活动不面向它们），不是故障。
# 2026-10-16 实测：36 个账号里只有 6 个 CN 账号返回 code=0，其余 30 个全 401。
# 所以按 realm 预先跳过，否则日志里会刷 30 行「失败」把真实问题埋掉。
CN_ONLY = True


def fetch_status(auth) -> tuple:
    """查补偿状态，返回 (ok, data_or_error)。"""
    st, d = do_post(auth, billing_base(auth), PATH_STATUS, {})
    if st != 200:
        return False, f"http={st} {str(d)[:160]}"
    if d.get("code") != 0:
        return False, f"code={d.get('code')} msg={d.get('msg')}"
    return True, (d.get("data") or {})


def claim(auth) -> tuple:
    """调用领取接口，返回 (ok, message)。"""
    st, d = do_post(auth, billing_base(auth), PATH_CLAIM, {})
    if st != 200:
        return False, f"http={st} {str(d)[:160]}"
    if d.get("code") != 0:
        return False, f"code={d.get('code')} msg={d.get('msg')}"
    return True, d.get("msg") or "OK"


def process(auth, dry_run: bool) -> tuple:
    """处理单个账号，返回 (状态词, 说明)。"""
    # 补偿活动只面向 CN 站；global/trae 账号打该端点必然 401，属预期而非故障。
    if CN_ONLY and auth_is_global(auth):
        return "skip", "非 CN 账号，该活动不适用"
    realm = (auth.get("realm") or "").strip().lower()
    if realm and realm not in ("cn", ""):
        return "skip", f"realm={realm}，该活动不适用"
    ok, status = fetch_status(auth)
    if not ok:
        return "fail", f"查状态失败：{status}"
    if not status.get("active"):
        return "skip", "无进行中的补偿活动"
    if status.get("claimed"):
        return "already", "本次补偿已领取过"
    credits = status.get("credit_num")
    deadline = status.get("end_time") or "?"
    if dry_run:
        return "dry", f"可领 {credits} 积分（截止 {deadline}）"
    ok, message = claim(auth)
    if not ok:
        return "fail", f"领取失败：{message}"
    return "success", f"已领取 {credits} 积分（截止 {deadline}）"


def main() -> int:
    parser = argparse.ArgumentParser(description="补偿积分领取（一次性任务）")
    parser.add_argument("--uid", help="只处理指定账号（uid 前缀或完整 uid）")
    parser.add_argument("--dry-run", action="store_true", help="只查状态，不领取")
    args = parser.parse_args()

    if args.uid:
        files = [f for f in all_auth_files() if args.uid in f]
        if not files:
            print(f"未找到匹配 {args.uid} 的账号")
            return 2
    else:
        files = all_auth_files()

    counts = {"success": 0, "already": 0, "skip": 0, "fail": 0, "dry": 0}
    for path in files:
        try:
            auth = load_auth(path)
        except Exception as error:
            print(f"{path.rsplit('/', 1)[-1][:20]}: 加载失败 {error}")
            counts["fail"] += 1
            continue
        uid = (auth.get("uid") or "")[:8]
        realm = auth.get("realm") or "?"
        try:
            kind, detail = process(auth, args.dry_run)
        except Exception as error:
            kind, detail = "fail", f"异常 {error!r}"
        counts[kind] = counts.get(kind, 0) + 1
        mark = {"success": "✓", "already": "=", "skip": "-",
                "fail": "✗", "dry": "?"}[kind]
        print(f"{mark} {uid} [{realm}] {detail}")

    total = len(files)
    print(f"\n补偿领取完成：共 {total}，成功 {counts['success']}，"
          f"已领 {counts['already']}，跳过 {counts['skip']}，失败 {counts['fail']}"
          + (f"，可领 {counts['dry']}" if args.dry_run else ""))
    return 1 if counts["fail"] else 0


if __name__ == "__main__":
    sys.exit(main())
