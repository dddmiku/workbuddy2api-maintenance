#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-10-11：初版——Trae CN 网页授权登录页与回调页。
"""trae_login.py — Trae CN 网页授权登录（面板内嵌页 + 回调接收）

为什么不需要本机助手
--------------------
Trae 授权页强制把回调地址设成 `http://127.0.0.1:<port>/authorize`，而浏览器网页
出于沙箱限制无法监听本机 TCP 端口，所以 Trae2api-cn 要求用户下载并运行一个本地
监听器（web_login.py）来接这个回调。多一个安装步骤，也多了"助手没启动"这一整类
故障。

本实现换成**用户手动回传**：授权完成后浏览器会停在一个打不开的 127.0.0.1 地址上，
但**地址栏里的查询串带着全部凭据**（userJwt / userInfo，或老流程的 refreshToken）。
用户把那一整条 URL 复制回来粘贴即可，凭据由服务器自己解析。

代价是每加一个账号要复制粘贴一次；换来的是零本机依赖——面板加号流程与既有
cn/global 扫码完全一致（生成链接 → 用户完成 → 回来点确认）。
"""

import json
import re
import time
import urllib.error
import urllib.parse
import urllib.request

TRAE_CLIENT_ID = "ono9krqynydwx5"
TRAE_AUTH_URL = "https://www.trae.cn/authorization"
TRAE_EXCHANGE_HOST = "https://api.trae.cn"
TRAE_EXCHANGE_PATH = "/cloudide/api/v3/trae/oauth/ExchangeToken"

# 授权页要求的本机回调地址：端口只是为了让 URL 合法，我们不监听它。
TRAE_CALLBACK_PORT = 8765
TRAE_CALLBACK_URL = "http://127.0.0.1:%d/authorize" % TRAE_CALLBACK_PORT


def _json_loads_safe(value):
    if not value:
        return {}
    try:
        parsed = json.loads(value)
    except (ValueError, TypeError):
        return {}
    return parsed if isinstance(parsed, dict) else {}


def build_auth_url(login_trace_id, machine_id, device_id):
    """拼出 Trae 网页授权 URL（参数与 Trae 官网一致）。"""
    params = {
        "login_version": "1",
        "auth_from": "solo",
        "login_channel": "native_ide",
        "plugin_version": "2.3.24254",
        "auth_type": "local",
        "client_id": TRAE_CLIENT_ID,
        "redirect": "0",
        "login_trace_id": login_trace_id,
        "auth_callback_url": TRAE_CALLBACK_URL,
        "machine_id": machine_id,
        "device_id": device_id,
        "x_device_id": device_id,
        "x_machine_id": machine_id,
        "x_device_brand": "Mac14,7",
        "x_device_type": "mac",
        "x_os_version": "macOS 26.4.1",
        "x_env": "",
        "x_app_version": "0.1.7",
        "x_app_type": "stable",
        "hide_saas_login": "true",
    }
    return TRAE_AUTH_URL + "?" + urllib.parse.urlencode(params)


def exchange_refresh_token(refresh_token, client_id=TRAE_CLIENT_ID, host=""):
    """用 refreshToken 兑换 Cloud-IDE-JWT（Trae 官网同款接口）。

    老授权流程的回调只带 refreshToken，需要这一步换取可用的 JWT。
    """
    if not refresh_token:
        return {}
    base = (host or TRAE_EXCHANGE_HOST).rstrip("/")
    url = base + TRAE_EXCHANGE_PATH
    payload = {
        "ClientID": client_id or TRAE_CLIENT_ID,
        "RefreshToken": refresh_token,
        "ClientSecret": "-",
        "UserID": "",
    }
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode("utf-8"), method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            body = resp.read().decode("utf-8")
    except urllib.error.HTTPError as error:
        detail = ""
        try:
            detail = error.read().decode("utf-8", "replace")[:500]
        except Exception:
            pass
        raise RuntimeError("ExchangeToken HTTP %s: %s" % (error.code, detail))
    except Exception as error:
        raise RuntimeError("ExchangeToken 请求失败: %s" % error)

    result = _json_loads_safe(body)
    data = result.get("Result") or result.get("result") or result
    if not isinstance(data, dict):
        raise RuntimeError("ExchangeToken 返回结构异常: %s" % body[:300])
    token = (data.get("Token") or data.get("token")
             or data.get("AccessToken") or data.get("accessToken") or "")
    if not token:
        raise RuntimeError("ExchangeToken 未返回 Token（refreshToken 可能已失效）: %s" % body[:300])
    user_info = _json_loads_safe(str(data.get("UserInfo") or data.get("userInfo") or ""))
    return {
        "token": token,
        "refreshToken": data.get("RefreshToken") or data.get("refreshToken") or refresh_token,
        "tokenExpireAt": str(data.get("TokenExpireAt") or data.get("tokenExpireAt") or ""),
        "userInfo": user_info,
        "clientId": data.get("ClientID") or data.get("clientId") or client_id,
        "host": base,
    }


def parse_callback(raw):
    """从用户粘贴的回调 URL（或裸查询串）里解析出 Trae 凭据。

    接受三种输入，因为用户复制到的东西形态不定：
      1. 完整回调 URL：http://127.0.0.1:8765/authorize?userJwt=...&userInfo=...
      2. 只有查询串：userJwt=...&userInfo=...
      3. 单个 refreshToken 串（老流程/手工只拿到这一项时）
    """
    text = (raw or "").strip()
    if not text:
        raise RuntimeError("没有收到内容，请把浏览器地址栏里的完整链接粘贴进来")

    query = {}
    if "://" in text or text.startswith("127.0.0.1") or "?" in text:
        # 按 URL 解析；解析不出查询串时（例如只是 refreshToken）走下面的兜底。
        parsed = urllib.parse.urlsplit(text if "://" in text else "http://" + text)
        query = dict(urllib.parse.parse_qsl(parsed.query))
    if not query:
        # 裸查询串形态：补一个假的 scheme 让 urlsplit 能解析出 query。
        parsed = urllib.parse.urlsplit("http://x/?" + text.lstrip("?"))
        query = dict(urllib.parse.parse_qsl(parsed.query))
    if not query:
        # 既不是 URL 也不是查询串：当作裸 refreshToken。
        if re.fullmatch(r"[A-Za-z0-9._~+/=-]{16,}", text):
            query = {"refreshToken": text}
        else:
            raise RuntimeError("无法从粘贴内容里解析出授权参数，请确认复制的是完整链接")

    return _credentials_from_query(query)


def _credentials_from_query(query):
    user_jwt = _json_loads_safe(query.get("userJwt") or query.get("user_jwt"))
    token = user_jwt.get("Token") or user_jwt.get("token") or ""
    refresh = (user_jwt.get("RefreshToken") or user_jwt.get("refreshToken")
               or query.get("refreshToken") or query.get("refresh_token")
               or query.get("data") or "")
    token_exp = user_jwt.get("TokenExpireAt") or user_jwt.get("tokenExpireAt") or ""
    user_info = _json_loads_safe(query.get("userInfo") or query.get("user_info"))
    user_id = (user_info.get("UserID") or user_info.get("userId")
               or user_info.get("userID") or query.get("userId") or "")
    region = user_info.get("Region") or user_info.get("region") or query.get("region") or "CN"
    client_id = (user_jwt.get("ClientID") or user_jwt.get("clientId")
                 or query.get("clientID") or query.get("clientId")
                 or query.get("client_id") or "")
    host = query.get("host") or user_info.get("Host") or user_info.get("host") or ""

    # 老流程：回调只带 refreshToken，必须兑换成 Cloud-IDE-JWT 才能用。
    if not token and refresh:
        exchanged = exchange_refresh_token(refresh, client_id or TRAE_CLIENT_ID, host)
        token = exchanged.get("token") or ""
        refresh = exchanged.get("refreshToken") or refresh
        token_exp = exchanged.get("tokenExpireAt") or token_exp
        user_info = exchanged.get("userInfo") or user_info
        user_id = user_id or exchanged.get("userInfo", {}).get("UserID", "")
        host = exchanged.get("host") or host

    if not token:
        raise RuntimeError("粘贴的内容里没有有效的 userJwt，请确认授权已完成")

    return {
        "access_token": token,
        "refresh_token": refresh,
        "token_expire_at": str(token_exp) if token_exp else "",
        "uid": user_id or "",
        "nickname": user_info.get("ScreenName") or user_info.get("screenName") or "",
        "region": region,
        "host": host or TRAE_EXCHANGE_HOST,
    }


def normalize_expire(raw):
    """把 TokenExpireAt（秒/毫秒时间戳字符串）归一成 Unix 秒；无法解析返回 0。"""
    text = str(raw or "").strip()
    if not text:
        return 0
    try:
        value = float(text)
    except (TypeError, ValueError):
        return 0
    if value > 1e12:  # 毫秒
        value /= 1000.0
    return int(value)
