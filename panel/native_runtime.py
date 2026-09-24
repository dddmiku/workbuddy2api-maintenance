#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：内置面板直接调用受限运行接口和同版辅助程序，不依赖宿主docker控制权限。
"""Restricted local runtime operations for the embedded administration panel."""
import collections
import json
import os
from pathlib import Path
import subprocess

import key_management


def run(args, timeout=60, check=False):
    args = list(args)
    container = os.environ.get('WB2API_CONTAINER', 'workbuddy2api')
    socket = os.environ['WB2API_ADMIN_SOCKET']
    if args == ['inspect', '-f', '{{.State.Running}}', container]:
        status, value = key_management.request(socket, 'GET', '/healthz', timeout=min(timeout, 10))
        return 0, 'true' if value.get('service') == 'workbuddy2api' else 'false', ''
    if args == ['restart', container]:
        status, value = key_management.request(socket, 'POST', '/runtime/reload', {}, timeout=timeout)
        if status == 200 and value.get('ok'):
            return 0, 'runtime reloaded', ''
        return 1, '', value.get('message') or '运行重载失败'
    if len(args) == 4 and args[0:2] == ['logs', '--tail'] and args[3] == container:
        try:
            count = max(1, min(5000, int(args[2])))
        except (TypeError, ValueError):
            return 1, '', '无效日志范围'
        rows = collections.deque(maxlen=count)
        log = Path(os.environ['WB2API_LOG_PATH'])
        for path in [Path(str(log) + '.2'), Path(str(log) + '.1'), log]:
            try:
                # Read bounded files produced by runlog; cap even if an operator
                # replaced one with a much larger diagnostic file.
                with path.open('rb') as file:
                    file.seek(0, os.SEEK_END)
                    size = file.tell()
                    file.seek(max(0, size - (8 << 20)))
                    if size > (8 << 20):
                        file.readline()
                    for line in file:
                        rows.append(line.decode('utf-8', errors='replace'))
            except FileNotFoundError:
                continue
        return 0, ''.join(rows), ''
    if len(args) >= 3 and args[:2] == ['exec', container]:
        command = args[2:]
        allowed = command == ['./credit']
        if command and command[0] == './login':
            allowed = len(command) in (3, 4) and command[1] in ('--realm=cn', '--realm=global') and command[-1] in ('url', 'poll')
            if len(command) == 4:
                import re
                allowed = allowed and bool(re.fullmatch(r'--session=[a-f0-9]{32}', command[2]))
        if not allowed:
            return 1, '', '不支持此运行操作'
        executable = Path(os.environ['WB2API_RUNTIME_DIR']) / command[0][2:]
        env = dict(os.environ)
        env['WB2A_AUTH_DIR'] = os.environ['WB2API_AUTHS_DIR']
        env['WB2API_LOGIN_STATE_DIR'] = str(Path(os.environ['WB2API_ADMIN_DIR']) / 'login-states')
        try:
            result = subprocess.run([str(executable), *command[1:]], cwd=os.environ['WB2API_GATEWAY_DIR'],
                                    env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True,
                                    timeout=timeout, check=False)
            if check and result.returncode:
                raise RuntimeError(result.stderr.strip() or '运行操作失败')
            return result.returncode, result.stdout.strip(), result.stderr.strip()
        except subprocess.TimeoutExpired:
            return 124, '', '运行操作超时'
        except OSError:
            return 127, '', '运行辅助程序不可用，请检查完整版本包'
    return 1, '', '不支持此运行操作'
