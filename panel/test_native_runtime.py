#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：验证跨进程文档更新不丢失、原生运行接口拒绝任意命令，并保持独立登录流程参数。
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import native_runtime


class NativeRuntimeTests(unittest.TestCase):
    def test_process_lock_preserves_concurrent_updates(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / 'credentials-fixture.json'
            target.write_text('{"count": 0}', encoding='utf-8')
            worker = '''import json,sys,time
from pathlib import Path
from file_lock import FileRLock
path=Path(sys.argv[1]);lock=FileRLock(lambda:str(path))
for i in range(40):
 with lock:
  with lock:
   value=json.loads(path.read_text())
   time.sleep(.002)
   value['count']+=1
   path.write_text(json.dumps(value))
'''
            children = [subprocess.Popen([sys.executable, '-c', worker, str(target)],
                        cwd=str(Path(__file__).resolve().parent), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                        for _ in range(2)]
            for child in children:
                out, err = child.communicate(timeout=20)
                self.assertEqual(child.returncode, 0, (out, err))
            self.assertEqual(json.loads(target.read_text())['count'], 80)

    def test_restricted_commands(self):
        env = {'WB2API_CONTAINER': 'fixture', 'WB2API_ADMIN_SOCKET': '/missing',
               'WB2API_RUNTIME_DIR': '/runtime/v2.2.0', 'WB2API_AUTHS_DIR': '/fixture/auths',
               'WB2API_ADMIN_DIR': '/fixture/panel-data', 'WB2API_GATEWAY_DIR': '/fixture'}
        with patch.dict(os.environ, env), patch.object(subprocess, 'run') as run:
            for command in [['exec','fixture','sh','-c','echo bad'], ['exec','fixture','./login','--realm=cn','--session=../bad','poll'],
                            ['exec','other','./credit'], ['remove','fixture']]:
                self.assertNotEqual(native_runtime.run(command)[0], 0)
            run.assert_not_called()
            run.return_value = subprocess.CompletedProcess([], 0, '[]', '')
            self.assertEqual(native_runtime.run(['exec','fixture','./credit'])[0], 0)
            self.assertEqual(Path(run.call_args[0][0][0]), Path('/runtime/v2.2.0/credit'))

    def test_exec_accepts_env_prefix_before_container(self):
        """面板给 ./credit 透传 WB2A_EXPIRING_SOON 时会带 -e 前缀。

        2026-10-10 踩到：native 模式判的是 args[:2] == ['exec', container]，
        而面板实际构造的是 ['exec','-e','K=V','<container>','./credit'] —— 前缀没剥
        就比对容器名，于是整条调用被拒（面板积分查询静默失败）。本用例锁住真实形状。
        """
        env = {'WB2API_CONTAINER': 'fixture', 'WB2API_ADMIN_SOCKET': '/missing',
               'WB2API_RUNTIME_DIR': '/runtime/v2.2.0', 'WB2API_AUTHS_DIR': '/fixture/auths',
               'WB2API_ADMIN_DIR': '/fixture/panel-data', 'WB2API_GATEWAY_DIR': '/fixture'}
        with patch.dict(os.environ, env), patch.object(subprocess, 'run') as run:
            run.return_value = subprocess.CompletedProcess([], 0, '[]', '')
            code, _, _ = native_runtime.run(
                ['exec', '-e', 'WB2A_EXPIRING_SOON=168h', 'fixture', './credit'])
            self.assertEqual(code, 0, "-e 前缀不该让 ./credit 被拒")
            argv = run.call_args[0][0]
            self.assertEqual(Path(argv[0]), Path('/runtime/v2.2.0/credit'))
            self.assertEqual(run.call_args[1]['env']['WB2A_EXPIRING_SOON'], '168h',
                             "透传的窗口必须真的进子进程环境")

    def test_exec_rejects_malformed_env_prefix(self):
        """-e 的值必须是合法 KEY=VALUE：畸形前缀不能被当成命令放行。"""
        env = {'WB2API_CONTAINER': 'fixture', 'WB2API_ADMIN_SOCKET': '/missing',
               'WB2API_RUNTIME_DIR': '/runtime/v2.2.0', 'WB2API_AUTHS_DIR': '/fixture/auths',
               'WB2API_ADMIN_DIR': '/fixture/panel-data', 'WB2API_GATEWAY_DIR': '/fixture'}
        with patch.dict(os.environ, env), patch.object(subprocess, 'run') as run:
            run.return_value = subprocess.CompletedProcess([], 0, '[]', '')
            for bad in (['exec', '-e', 'NOEQUALS', 'fixture', './credit'],
                        ['exec', '-e', '1BAD=x', 'fixture', './credit'],
                        ['exec', '-e', 'WB2A_EXPIRING_SOON=1h', 'other', './credit'],
                        ['exec', '-v', '/:/host', 'fixture', './credit']):
                self.assertNotEqual(native_runtime.run(bad)[0], 0, "应拒绝 %r" % (bad,))
            run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
