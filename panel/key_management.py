#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：内置面板优先使用网关传入的已解析管理socket，避免自定义配置路径或环境覆盖造成分叉。
# 2026-09-22：密钥管理默认启用——api_keys_file 留空（含历史示例里的空串）时按默认
#             路径解析，只有显式 api_keys_enabled=false 才关闭；修掉新装用户照抄
#             config.example.json 时密钥页报「密钥管理尚未启用」、建不了密钥。
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
    if os.environ.get("WB2API_RUNTIME") == "native" and os.environ.get("WB2API_ADMIN_SOCKET"):
        return os.environ["WB2API_ADMIN_SOCKET"]
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
    """解析生效的密钥库路径；None 表示多密钥管理被显式关闭。

    与网关 applyAPIKeysDefault 保持同一口径：
    - 显式 `api_keys_enabled=false` → 关闭；
    - 写了路径 → 用它；
    - 其余（没写、写空串、null）→ 默认路径。

    空串按「没配」处理是有意的：历史 config.example.json 里这一项就是空串，
    照抄它的部署很多，把它当成关闭信号正是「密钥管理尚未启用」这条提示的来源。
    启动时库里一把密钥都没有不是问题，用户随后在面板里创建即可。
    """
    if config.get("api_keys_enabled") is False:
        return None
    path = config.get("api_keys_file")
    if isinstance(path, str) and path.strip():
        return path
    return DEFAULT_API_KEYS_FILE


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
