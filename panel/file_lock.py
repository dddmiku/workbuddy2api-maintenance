#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：热交接中的两个面板进程共享重入文件锁，避免改密/退出撤销及配置写入互相覆盖。
"""Reentrant process and thread lock for the native panel's shared documents."""
import os
import threading


class FileRLock:
    def __init__(self, path):
        self.path = path
        self.thread = threading.RLock()
        self.local = threading.local()

    def __enter__(self):
        self.thread.acquire()
        depth = getattr(self.local, 'depth', 0)
        try:
            if depth == 0:
                path = self.path() + '.lock'
                os.makedirs(os.path.dirname(os.path.abspath(path)), mode=0o700, exist_ok=True)
                handle = open(path, 'a+b')
                try:
                    if os.name == 'nt':
                        import msvcrt
                        handle.seek(0)
                        msvcrt.locking(handle.fileno(), msvcrt.LK_LOCK, 1)
                    else:
                        import fcntl
                        fcntl.flock(handle, fcntl.LOCK_EX)
                except BaseException:
                    handle.close()
                    raise
                self.local.handle = handle
            self.local.depth = depth + 1
            return self
        except BaseException:
            self.thread.release()
            raise

    def __exit__(self, *_):
        try:
            self.local.depth -= 1
            if self.local.depth == 0:
                handle = self.local.handle
                try:
                    if os.name == 'nt':
                        import msvcrt
                        handle.seek(0)
                        msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
                    else:
                        import fcntl
                        fcntl.flock(handle, fcntl.LOCK_UN)
                finally:
                    handle.close()
        finally:
            self.thread.release()


def document_lock(path):
    return FileRLock(path) if os.environ.get('WB2API_RUNTIME') == 'native' else threading.RLock()
