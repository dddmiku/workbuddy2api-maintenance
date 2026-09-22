#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-22：api_keys_file 未配置时按默认路径解析（与网关同口径），修掉新装用户
#             照抄 config.example.json 时密钥页报「密钥管理尚未启用」。
# 2026-09-16：通过权限受限的本机 Unix socket 调用网关管理接口，避免使用可分发的调用密钥作为管理凭证。
"""Private transport shared by the authenticated panel and gateway."""

import http.client
import json
import os
import socket

# 密钥库默认位置，必须与网关 cmd/server/config.go 的 DefaultAPIKeysFile 一致。
# 面板读的是原始 config.json，网关读的是归一化后的配置；两边默认值一旦分叉，
# 就会出现「网关已启用、面板却报未启用」。
DEFAULT_API_KEYS_FILE = "./data/api_keys.json"


def socket_path(config_path, base):
    with open(config_path, "r", encoding="utf-8") as file:
        config = json.load(file)
    registry = _api_keys_file(config)
    if not registry:
        return None
    path = config.get("api_keys_socket") or os.path.join(os.path.dirname(registry), "api_keys.sock")
    if path.startswith("/app/"):
        path = os.path.join(base, path[5:])
    return path if os.path.isabs(path) else os.path.normpath(os.path.join(base, path))


def _api_keys_file(config):
    """解析生效的密钥库路径；None/空串表示多密钥管理未启用。

    与网关 applyAPIKeysDefault 保持同一口径：
    - 显式写了路径 → 用它；
    - 显式写 "" → 管理员主动关闭；
    - 没写（或 null）→ 默认启用，但仅当 api_key 非空。api_key 也为空时网关会
      保持旧的不鉴权模式（否则空密钥库会把原本免鉴权的部署锁成全部 401），
      面板必须跟着报「未启用」，不能显示成可用。
    """
    if "api_keys_file" in config and config["api_keys_file"] is not None:
        return config["api_keys_file"]
    if str(config.get("api_key") or "").strip():
        return DEFAULT_API_KEYS_FILE
    return None


def request(path, method, endpoint, body=None, timeout=15):
    if not path:
        return 503, {"ok": False, "message": "密钥管理尚未启用"}
    connection = UnixConnection(path, timeout)
    try:
        data = None if body is None else json.dumps(body, ensure_ascii=False).encode("utf-8")
        connection.request(method, endpoint, body=data, headers={"Content-Type": "application/json"})
        response = connection.getresponse()
        raw = response.read((1 << 20) + 1)
        if len(raw) > 1 << 20:
            raise ValueError("management response exceeds limit")
        result = json.loads(raw.decode("utf-8"))
        if not isinstance(result, dict):
            raise ValueError("invalid management response")
        return response.status, result
    except (OSError, http.client.HTTPException, ValueError):
        return 503, {"ok": False, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
    finally:
        connection.close()


class UnixConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)
