#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：生成完整运行包清单及确定文件集的压缩包，排除配置、凭据和运行数据。
"""Package an already built runtime; this utility never reads deployment data."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import tarfile

FILES = ('wb2api', 'login', 'credit', 'signin_bin', 'trial_bin', 'activity_bin',
         'login.sh', 'credit.sh', 'signin.sh', 'trial.sh',
         'scripts/global_region.py', 'scripts/task_common.py', 'scripts/task_runner.py',
         'scripts/school_open_day_2026.py', 'scripts/growth_center.py')


def package(stage, version, arch, archive=None):
    stage = Path(stage)
    arch = {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(arch, arch)
    if arch not in ('amd64', 'arm64'):
        raise ValueError('unsupported runtime architecture')
    files = {}
    for name in FILES:
        path = stage / name
        if not path.is_file() or path.is_symlink():
            raise ValueError('missing regular runtime file: ' + name)
        files[name] = hashlib.sha256(path.read_bytes()).hexdigest()
    manifest = {'format': 1, 'version': version, 'os': 'linux', 'arch': arch, 'files': files}
    (stage / 'manifest.json').write_text(json.dumps(manifest, sort_keys=True, indent=2) + '\n', encoding='utf-8', newline='\n')
    if archive:
        with tarfile.open(archive, 'w:gz') as bundle:
            for name in ('manifest.json', *FILES):
                data = (stage / name).read_bytes()
                item = tarfile.TarInfo(name)
                item.size = len(data)
                item.mode = 0o755 if '/' not in name and name != 'manifest.json' else 0o644
                bundle.addfile(item, io.BytesIO(data))
    return manifest


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--stage', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--arch', required=True)
    parser.add_argument('--archive')
    args = parser.parse_args()
    package(args.stage, args.version, args.arch, args.archive)


if __name__ == '__main__':
    main()
