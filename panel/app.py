#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-26：面板版本同步为 2.4.2（输出预算与全项目审查修复）。
# 2026-09-25：无筛选条件直接使用空参数列表，兼容Python3.8严格解析空查询串的差异。
# 2026-09-25：面板版本同步为2.4.0，限流、调度消费明细与兼容性回归作为同一运行版本交付。
# 2026-09-25：接入请求明细和限流私有通道，验证密钥限流字段；账本不可读时不再显示零消费。
# 2026-09-25：面板版本同步为2.3.0，对应新增Gemini与四协议统一校验的整包版本。
# 2026-09-25：最终统一运行版本为2.2.1，包含真实SDK审查发现的工具事件与业务JSON保真修复。
# 2026-09-25：每次扫码使用独立登录流程ID，原生重载按进程就绪确认，不把账号冷却误报成重启失败。
# 2026-09-25：管理台支持网关托管的私有Unix监听与原生运行接口，配置和账号路径由网关统一传入。
# 2026-09-25：请求日志保留网关生成的请求标识，便于对应客户端错误与上游请求。
# 2026-09-25：控制台版本同步上下文恢复修复 2.1.29。
# 2026-09-24：版本号提升到 2.1.28（上下文裁剪回真体积 + 超限幅度上限）。
# 2026-09-24：提前拒绝后限时丢弃迟到的小请求体，保留403响应并避免未读字节触发连接重置。
# 2026-09-24：登录、续期和改密绑定已验证的凭据版本；并发登录先占用尝试预算，避免绕过限流。
# 2026-09-23：版本号提升到 2.1.26（来源级限流收敛 + 密钥级 global→CN 回落 +
#             加号写入权限竞态修复）。
# 2026-09-23：修掉加号后「账号没加载」的权限竞态：先把临时文件 chown/chmod 再
#             os.replace 落地，避免落地瞬间文件仍是 root:0600、网关（uid 10001）
#             读不到而被静默跳过（实测 15:52:05 只加载 23 个账号，10 秒后恢复 24）。
#             chown 失败不再静默吞掉：写成 stderr 告警，避免同类问题再次无声。
# 2026-09-23：版本号提升到 2.1.25（网关侧修复两行/三行极短行交替循环漏检）。
# 2026-09-23：版本号提升到 2.1.24（网关侧新增上下文超限自动裁剪重发）。
# 2026-09-23：日志页的 60/120/300/600 改为「请求条数」语义：按请求行数取窗口，
#             不再是 docker --tail 的原始行数（日志里 WARN 行会占掉一半，按钮写
#             60 实际只出 ~30 条请求）。
# 2026-09-22：密钥管理默认启用：api_keys_file 留空也按默认路径解析，只有显式
#             api_keys_enabled=false 才关闭；修掉新装用户建不了密钥的问题。
# 2026-09-22：模型绑定收紧为「完整模型名逐字相等」，创建与编辑都在表单侧拒掉裸名，
#             与网关的写入校验对齐（绑定必须是 cn:/global: 开头的完整模型名）。
# 2026-09-22：登录限流分桶只采信可信代理（WB2API_TRUSTED_PROXIES）转发的 IP，
#             并对分桶表加上限——此前转发头无条件采信，轮换一个头就换一个桶，
#             限流可被绕过且桶表能被撑到任意大小。
# 2026-09-22：新增 /api/features/reasoning-loop，转发网关的「命中循环只停不重发」热切换；
#             GET 读当前值，POST 改值，改完立即作用于后续请求，不需要重启网关。
# 2026-09-20：2.1.9 重复推理保护扩到正文（上游修复同步到面板文案）。
# 2026-09-20：2.1.8 密钥列表合并累计 token 用量，创建/编辑支持有效期（默认无限制）。
# 2026-09-20：2.1.4 控制台按 new-api 面板规范重做外观（顶栏横跨、浅色侧栏、azure 主色、
# 2026-09-20：2.1.4 控制台按 new-api 面板规范重做外观（顶栏横跨、浅色侧栏、azure 主色、
#             卡片 14px 圆角），并把登录会话有效期从 12 小时改为 30 天。
# 2026-09-20：2.1.3 冷却剩余带实时倒计时（状态列每秒就地刷新，归零自动补取数据）。
# 2026-09-20：2.1.2 账号状态列显示冷却剩余与原因（不再把凭证有效期摆在「冷却中」旁），
#             最近活动区分「无成功记录」与「无记录」，并透出冷却台账字段。
# 2026-09-20：2.1.1 密钥列表支持按需复制与逐密钥重复推理保护，沿用管理员会话和来源校验。
# 2026-09-15: 初版。workbuddy2api 账号管理面板后端：
#   扫码加号(OAuth url/poll)、启用/禁用(改名 .disabled)、删除(移入回收站)、
#   账号池状态与积分聚合、容器重启。网关自身无管理接口，故独立成服务。
# 2026-09-16: 内建登录层，撤掉 nginx basic auth：
#   自带 /login 登录页与会话 cookie（滑动过期，现为 30 天）、登录失败按 IP 限流、
#   账密在线修改（用户名 + 密码），旧 htpasswd 的 $apr1$ 口令继续可校验。
# 2026-09-16：新增登录保护的多密钥管理路由，使用本机管理通道、严格请求校验并隐藏配置中的完整密钥。
# 2026-09-17：路径、端口与容器名支持环境变量覆盖，便于与网关同一 Compose 项目部署。
# 2026-09-17：密钥管理支持模型绑定字段，并新增供前端选择模型的 /api/models。
# 2026-09-17：新增用量统计通道 /api/usage；容器日志解析成结构化请求行供日志页表格展示。
# 2026-09-17：新增热更新通道：/api/update 读状态，/api/update/apply 触发版本切换。
# 2026-09-18：统一管理写请求的来源、格式和大小校验；修复退出时续期、撤销丢失、根路径登录和损坏凭证被覆盖。
# 2026-09-18：严格验证开关与标识，日志同时保留 stdout/stderr，避免错误输入触发操作或隐藏诊断。
# 2026-09-18：账号凭据先安全落盘再清理旧文件，启停冲突保留双方，回收文件使用唯一名称。
# 2026-09-18：重启等待覆盖容器停止宽限，并用真实健康响应确认成功，超时不再报已加载账号。
# 2026-09-18：滑动续期保留会话标识，退出撤销同一会话的旧副本，防止续期前令牌复活。
# 2026-09-19：积分刷新共享同一次查询，失败保留旧余额并显示错误、延迟重试，避免永久等待和旧查询覆盖新结果。

"""workbuddy2api 账号管理面板 —— 后端

只监听 127.0.0.1，经 nginx 暴露到 /admin/。登录与本面板账号由本进程自己管，
凭证落在同目录 credentials.json，首次启动从 /etc/nginx/.htpasswd_wb2admin 继承
用户名与 apr1 口令哈希，因此原有密码不作废。依赖：仅标准库 + docker CLI。
"""

import base64
import hashlib
import hmac
import ipaddress
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import threading
import socketserver
import signal
import time
import key_management
from file_lock import document_lock
from http.cookies import SimpleCookie
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen
from urllib.parse import parse_qsl, unquote, urlencode, urlsplit

HERE = os.path.dirname(os.path.abspath(__file__))
# 部署形态：宿主机 systemd（默认值）或与网关同项目的容器（由环境变量覆盖）。
# BASE 之外的目录都随 BASE 走，容器里把网关目录整体挂到 /gateway 即可。
BASE = os.environ.get("WB2API_GATEWAY_DIR", "/opt/workbuddy2api")
AUTH_DIR = os.environ.get("WB2API_ADMIN_DIR", HERE)
AUTHS_DIR = os.environ.get("WB2API_AUTHS_DIR", os.path.join(BASE, "auths"))
TRASH_DIR = os.path.join(BASE, "auths-trash")
CONFIG_PATH = os.environ.get("WB2API_CONFIG_PATH", os.path.join(BASE, "config.json"))
INDEX_PATH = os.path.join(HERE, "index.html")
LOGIN_PATH = os.path.join(HERE, "login.html")
CRED_PATH = os.path.join(AUTH_DIR, "credentials.json")
HTPASSWD_PATH = os.environ.get("WB2API_HTPASSWD_PATH", "/etc/nginx/.htpasswd_wb2admin")

# 会话滑动过期：30 天。管理台是长期挂着的运维界面，12 小时会让人一天里反复登录；
# 续期仍按剩余不足 1/3（即少于 10 天）时滑动刷新，令牌本身不换新。
SESSION_TTL = 30 * 24 * 3600
PBKDF2_ROUNDS = 200000
PBKDF2_PREFIX = "pbkdf2_sha256"
LOGIN_MAX_FAILS = 6              # 同一 IP 在窗口内的失败次数
LOGIN_WINDOW = 300.0
# 登录限流分桶表的容量上限。分桶键来自转发头，未鉴权的攻击者能造出任意多的键，
# 没有上限就是一条内存增长面；超过后清掉最旧的一批，宁可短暂放宽限流也不被撑爆。
LOGIN_BUCKET_LIMIT = 10000
COOKIE_NAME = "wb2a_admin"

# 可信反向代理网段（CIDR，逗号分隔）。只有直连对端落在这些网段内，才采信
# X-Real-IP / CF-Connecting-IP / X-Forwarded-For —— 这些头客户端能自己伪造，
# 不做来源校验就等于把限流分桶键交给攻击者（轮换一个头就换一个桶）。
#
# 默认覆盖回环与私网：文档推荐的部署是「nginx 在同一台机器或同一个 Compose 网络
# 里转发到面板」，那种形态下对端必然是回环或容器网段。代理在别的地址时加进来。
TRUSTED_PROXIES_RAW = os.environ.get(
    "WB2API_TRUSTED_PROXIES",
    "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7")

CONTAINER = os.environ.get("WB2API_CONTAINER", "workbuddy2api")
PANEL_VERSION = "2.4.2"

# 网关请求行（logging.go 的表格日志）：
# | #012 | 22:04:21 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | TTFB=3414ms | in=306401 | hit=298112 | tok=110 | 34.3tok/s | total=3.4s |
# in=/hit= 是 2026-09-18 新增列（输入 tokens 与其中缓存命中数）；旧行没有这两列，正则按可选取。
REQUEST_ROW = re.compile(
    r"^\|\s*#(?P<seq>\d+)\s*\|\s*(?P<time>[^|]*?)\s*\|\s*(?P<model>[^|]*?)\s*\|\s*(?P<mode>[^|]*?)\s*\|\s*"
    r"(?P<status>\d+)\s*\|\s*(?:key=(?P<key>[^|]*?)\s*\|\s*)?uid=(?P<uid>[^|]*?)\s*\|\s*TTFB=(?P<ttfb>[^|]*?)\s*\|\s*"
    r"(?:in=(?P<in>[^|]*?)\s*\|\s*hit=(?P<hit>[^|]*?)\s*\|\s*)?"
    r"tok=(?P<tok>[^|]*?)\s*\|\s*(?P<rate>[^|]*?)\s*\|\s*total=(?P<total>[^|]*?)\s*\|\s*(?:rid=(?P<request_id>[A-Za-z0-9_]+)\s*\|\s*)?$"
)


def parse_request_log(text):
    """把请求行解析成表格行；其余行（WARN/ERR 等）单独返回，保持原始顺序。"""
    rows, other = [], []
    for line in (text or "").splitlines():
        match = REQUEST_ROW.match(line.strip())
        if not match:
            if line.strip():
                other.append(line)
            continue
        item = match.groupdict()
        item["key"] = (item["key"] or "").strip() or "-"
        for field in ("in", "hit"):
            item[field] = (item.get(field) or "").strip() or "-"
        rows.append(item)
    return rows, other[-60:]


# LOG_TAIL_FACTOR 决定为了拿到 N 条请求要往回取多少原始行。
# 请求行与 WARN/ERR 行混排，比例不固定；实测噪声重时请求约占一半，
# 取 4 倍再留 200 行余量，既能凑够目标条数又不会把 tail 拉得过大。
LOG_TAIL_FACTOR = 4
LOG_TAIL_HEADROOM = 200
LOG_TAIL_MAX = 5000


def request_window_size(want):
    """want 条请求需要往回取多少原始日志行。"""
    return min(want * LOG_TAIL_FACTOR + LOG_TAIL_HEADROOM, LOG_TAIL_MAX)
GATEWAY = os.environ.get("WB2API_GATEWAY_URL", "http://127.0.0.1:7863")
LISTEN_HOST = os.environ.get("WB2API_ADMIN_HOST", "127.0.0.1")
try:
    LISTEN_PORT = int(os.environ.get("WB2API_ADMIN_PORT", "7864"))
except ValueError:
    LISTEN_PORT = 7864

REALMS = ("cn", "global")
UID_RE = re.compile(r"^[A-Za-z0-9_-]{1,80}$")
AUTH_FILE_RE = re.compile(r"^workbuddy-(?P<uid>.+?)\.json(?P<disabled>\.disabled)?$")

# 首屏读取缓存，手动刷新等待同一次后台查询；失败不丢弃最后一次成功结果。
CREDIT_TTL = 60.0
CREDIT_RETRY_INTERVAL = 30.0
_credit_cache = {"ts": 0.0, "data": None, "error": None, "retry_at": 0.0}
_credit_refreshing = None
_lock = document_lock(lambda: CONFIG_PATH)


# ═══════════════════════════════════════════════════════════════════════
# 登录层：口令哈希、凭证文件、签名会话、限流
# ═══════════════════════════════════════════════════════════════════════

ITOA64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
_cred_lock = document_lock(lambda: CRED_PATH)
_fails = {}          # ip -> [timestamp, ...]
_revoked = set()     # 已签出但被主动作废的 nonce


def _to64(value, count):
    out = []
    for _ in range(count):
        out.append(ITOA64[value & 0x3F])
        value >>= 6
    return "".join(out)


def apr1_crypt(password, salt):
    """Apache apr1（MD5-crypt）口令。与 htpasswd -m / openssl passwd -apr1 等价。

    面板接手前，密码由 nginx basic auth 的 .htpasswd 管；为了不把老密码作废，
    首启用同一段算法校验存量哈希。
    """
    pw = password.encode("utf-8")
    sl = salt.encode("utf-8")[:8]
    magic = b"$apr1$"
    ctx = hashlib.md5(pw + magic + sl)
    alt = hashlib.md5(pw + sl + pw).digest()
    n = len(pw)
    i = n
    while i > 0:
        ctx.update(alt[:16] if i > 16 else alt[:i])
        i -= 16
    i = n
    while i:
        ctx.update(b"\x00" if (i & 1) else pw[:1])
        i >>= 1
    digest = ctx.digest()
    for i in range(1000):
        c = hashlib.md5()
        c.update(pw if (i & 1) else digest)
        if i % 3:
            c.update(sl)
        if i % 7:
            c.update(pw)
        c.update(digest if (i & 1) else pw)
        digest = c.digest()
    out = ""
    for a, b, cc in ((0, 6, 12), (1, 7, 13), (2, 8, 14), (3, 9, 15), (4, 10, 5)):
        out += _to64(digest[a] << 16 | digest[b] << 8 | digest[cc], 4)
    out += _to64(digest[11], 2)
    return "$apr1$" + salt[:8] + "$" + out


def parse_htpasswd(path):
    """读 .htpasswd，返回 (用户名, 原始哈希) 或 (None, None)。"""
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            for line in fh:
                line = line.strip()
                if not line or line.startswith("#") or ":" not in line:
                    continue
                name, _, digest = line.partition(":")
                if name.strip() and digest.strip():
                    return name.strip(), digest.strip()
    except OSError:
        pass
    return None, None


def hash_password(password):
    salt = secrets.token_bytes(16)
    dk = hashlib.pbkdf2_hmac("sha256", password.encode("utf-8"), salt, PBKDF2_ROUNDS)
    return "%s$%d$%s$%s" % (
        PBKDF2_PREFIX, PBKDF2_ROUNDS,
        base64.b64encode(salt).decode("ascii"),
        base64.b64encode(dk).decode("ascii"))


def verify_password(password, stored):
    """校验明文口令。stored 支持 pbkdf2_sha256$... 与 $apr1$... 两种。"""
    if not stored:
        return False
    if stored.startswith("$apr1$"):
        parts = stored.split("$")
        if len(parts) < 4:
            return False
        salt = parts[2][:8]
        try:
            return hmac.compare_digest(apr1_crypt(password, salt), stored)
        except Exception:
            return False
    if stored.startswith(PBKDF2_PREFIX + "$"):
        try:
            _, rounds, salt_b64, hash_b64 = stored.split("$", 3)
            salt = base64.b64decode(salt_b64)
            want = base64.b64decode(hash_b64)
            got = hashlib.pbkdf2_hmac("sha256", password.encode("utf-8"),
                                      salt, int(rounds))
            return hmac.compare_digest(got, want)
        except Exception:
            return False
    return False


def _new_credentials():
    """没有任何存量凭证时的兜底：随机口令写进 initial-password.txt，避免锁死。"""
    pw = secrets.token_urlsafe(12)
    doc = {
        "version": 1,
        "username": "wbadmin",
        "password": hash_password(pw),
        "sessionKey": secrets.token_hex(32),
        "updatedAt": int(time.time()),
        "history": [],
    }
    _save_credentials(doc)
    hint = os.path.join(AUTH_DIR, "initial-password.txt")
    try:
        with open(hint, "w", encoding="utf-8") as fh:
            fh.write("username: wbadmin\npassword: %s\n" % pw)
        os.chmod(hint, 0o600)
    except OSError:
        pass
    sys.stderr.write("未找到凭证，已生成初始账号，见 %s\n" % hint)
    return doc


def _save_credentials(doc):
    directory = os.path.dirname(os.path.abspath(CRED_PATH))
    os.makedirs(directory, mode=0o700, exist_ok=True)
    descriptor, tmp = tempfile.mkstemp(prefix=".credentials-", dir=directory)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as fh:
            json.dump(doc, fh, indent=2, ensure_ascii=False)
            fh.write("\n")
            fh.flush()
            os.fsync(fh.fileno())
        os.chmod(tmp, 0o600)
        os.replace(tmp, CRED_PATH)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def load_credentials():
    """读凭证；文件不存在时从 nginx 的 .htpasswd 继承用户名与 apr1 哈希。"""
    with _cred_lock:
        missing = False
        try:
            with open(CRED_PATH, "r", encoding="utf-8") as fh:
                doc = json.load(fh)
        except FileNotFoundError:
            doc = None
            missing = True
        if not missing:
            if not isinstance(doc, dict) or not all(isinstance(doc.get(name), str) and doc[name]
                                                    for name in ("username", "password")):
                raise ValueError("登录凭证文件无效，请恢复已有备份")
            if not doc.get("sessionKey"):
                doc["sessionKey"] = secrets.token_hex(32)
                _save_credentials(doc)
            if not isinstance(doc["sessionKey"], str) or not re.fullmatch(r"[0-9a-fA-F]{64}", doc["sessionKey"]):
                raise ValueError("登录凭证的会话密钥无效，请恢复已有备份")
            if not isinstance(doc.get("revokedSessions", {}), dict):
                raise ValueError("登录会话撤销记录无效，请恢复已有备份")
            return doc

        name, digest = parse_htpasswd(HTPASSWD_PATH)
        if name and digest:
            doc = {
                "version": 1,
                "username": name,
                "password": digest,
                "sessionKey": secrets.token_hex(32),
                "updatedAt": int(time.time()),
                "history": [],
                "inherited": True,
            }
            _save_credentials(doc)
            sys.stderr.write("已从 %s 继承登录口令（用户名 %s）\n"
                             % (HTPASSWD_PATH, name))
            return doc
        return _new_credentials()


def _b64u(raw):
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def _unb64u(text):
    pad = "=" * (-len(text) % 4)
    return base64.urlsafe_b64decode(text + pad)


def issue_session(username, ttl=SESSION_TTL, nonce=None, credentials=None):
    """签名会话令牌：payload.nonce + HMAC。key 落盘，面板重启后仍有效。"""
    doc = load_credentials() if credentials is None else credentials
    nonce = nonce or secrets.token_hex(12)
    payload = {"u": username, "e": int(time.time()) + ttl, "n": nonce}
    body = _b64u(json.dumps(payload, separators=(",", ":")).encode("utf-8"))
    key = bytes.fromhex(doc["sessionKey"])
    sig = _b64u(hmac.new(key, body.encode("ascii"), hashlib.sha256).digest())
    return body + "." + sig, payload


def read_session(token, credentials=None):
    """校验令牌。返回 payload 或 None。"""
    if not token or "." not in token:
        return None
    body, _, sig = token.partition(".")
    doc = load_credentials() if credentials is None else credentials
    try:
        key = bytes.fromhex(doc["sessionKey"])
        want = hmac.new(key, body.encode("ascii"), hashlib.sha256).digest()
        if not hmac.compare_digest(want, _unb64u(sig)):
            return None
        payload = json.loads(_unb64u(body).decode("utf-8"))
    except Exception:
        return None
    if not isinstance(payload, dict):
        return None
    if not isinstance(payload.get("n"), str) or not re.fullmatch(r"[0-9a-f]{24}", payload["n"]):
        return None
    if payload.get("n") in _revoked or payload.get("n") in doc.get("revokedSessions", {}):
        return None
    expires = payload.get("e")
    if isinstance(expires, bool) or not isinstance(expires, (int, float)) or expires < time.time():
        return None
    if payload.get("u") != doc.get("username"):
        return None
    return payload


def _same_credentials(first, second):
    return all(first.get(field) == second.get(field)
               for field in ('username', 'password', 'sessionKey'))


def _client_ip(handler):
    """真实客户端 IP。

    线上是 Cloudflare -> nginx -> 面板：nginx 用 real_ip 模块把 $remote_addr
    还原成访客 IP 并写进 X-Real-IP，这个头由 nginx 覆盖、客户端伪造不了。

    但这三个头本身都是普通请求头：只要直连对端不是可信代理，客户端就能自带任意值。
    因此这里先看直连对端（client_address）是否落在 WB2API_TRUSTED_PROXIES 内——
    不在就一律用直连地址分桶，转发头完全忽略。否则攻击者轮换一个 X-Real-IP /
    CF-Connecting-IP 就换一个限流桶，登录限流形同虚设。

    登录限流按这里分桶：取错会把不相干的人关进同一个小黑屋，或者让人随便换头
    就绕开限流。
    """
    peer = handler.client_address[0] if handler.client_address else ""
    if not _is_trusted_proxy(peer):
        return peer or "?"
    for header in ("X-Real-IP", "CF-Connecting-IP", "X-Forwarded-For"):
        raw = handler.headers.get(header)
        if raw:
            candidate = raw.split(",")[0].strip()
            if candidate:
                return candidate
    return peer or "?"


def _trusted_proxies():
    """解析 WB2API_TRUSTED_PROXIES 为网段列表；非法项跳过并在启动日志里提示。"""
    networks = []
    for item in (TRUSTED_PROXIES_RAW or "").split(","):
        item = item.strip()
        if not item:
            continue
        try:
            networks.append(ipaddress.ip_network(item, strict=False))
        except ValueError:
            sys.stderr.write("忽略无法解析的可信代理网段: %r" % item + chr(10))
    return networks


_TRUSTED_PROXY_NETWORKS = _trusted_proxies()


def _is_trusted_proxy(peer):
    """直连对端是否落在可信代理网段内。空/非法地址按不可信处理。"""
    if not peer:
        return False
    try:
        address = ipaddress.ip_address(peer)
    except ValueError:
        return False
    for network in _TRUSTED_PROXY_NETWORKS:
        if address.version == network.version and address in network:
            return True
    return False


def login_blocked(ip):
    now = time.time()
    with _cred_lock:
        hits = [t for t in _fails.get(ip, []) if now - t < LOGIN_WINDOW]
        if hits:
            _fails[ip] = hits
        else:
            # 窗口内没有失败记录：把桶删掉而不是留一个空列表。
            # 此前是 `_fails[ip] = hits`，于是每个出现过的 IP 都会永久留下一个空桶，
            # 未鉴权的登录端点可以被用来把这张表撑到任意大小。
            _fails.pop(ip, None)
        return len(hits) >= LOGIN_MAX_FAILS, len(hits)


def login_failed(ip):
    with _cred_lock:
        _trim_login_buckets_locked()
        _fails.setdefault(ip, []).append(time.time())


def login_ok(ip):
    with _cred_lock:
        _fails.pop(ip, None)


def _trim_login_buckets_locked():
    """桶数超过上限时，按「最后失败时间」丢掉最旧的一批（调用方须持 _cred_lock）。"""
    if len(_fails) <= LOGIN_BUCKET_LIMIT:
        return
    ordered = sorted(_fails.items(), key=lambda item: max(item[1]) if item[1] else 0.0)
    # 一次清掉 1/8，避免刚好卡在上限时每个请求都排一次序。
    drop = max(1, LOGIN_BUCKET_LIMIT // 8)
    for key, _ in ordered[:drop]:
        _fails.pop(key, None)


def api_key():
    try:
        with open(CONFIG_PATH, "r", encoding="utf-8") as f:
            return json.load(f).get("api_key", "") or ""
    except Exception:
        return ""


def docker(args, timeout=60, check=False):
    if os.environ.get("WB2API_RUNTIME") == "native":
        import native_runtime
        return native_runtime.run(args, timeout=timeout, check=check)
    try:
        p = subprocess.run(["docker"] + list(args), capture_output=True,
                           text=True, timeout=timeout)
        out, err = p.stdout.strip(), p.stderr.strip()
        if check and p.returncode != 0:
            raise RuntimeError(err or out or ("docker 退出码 %d" % p.returncode))
        return p.returncode, out, err
    except subprocess.TimeoutExpired:
        return 124, "", "docker 命令超时(%ds)" % timeout
    except FileNotFoundError:
        return 127, "", "找不到 docker 命令"


def container_running():
    rc, out, _ = docker(["inspect", "-f", "{{.State.Running}}", CONTAINER], timeout=20)
    return rc == 0 and out.strip() == "true"


def gateway_get(path, timeout=15):
    try:
        management_socket = key_management.socket_path(CONFIG_PATH, BASE)
        if management_socket:
            code, payload = key_management.request(management_socket, "GET", path, timeout=timeout)
            return payload if code < 400 else None
    except (OSError, ValueError):
        return None
    key = api_key()
    req = Request(GATEWAY + path, headers={"Authorization": "Bearer " + key})
    try:
        with urlopen(req, timeout=timeout) as r:
            return json.loads(r.read().decode("utf-8"))
    except HTTPError as e:
        try:
            return json.loads(e.read().decode("utf-8"))
        except Exception:
            return None
    except (URLError, OSError, ValueError):
        return None


TASK_ENABLE_KEY = {
    "checkin": "checkin_enabled",
    "travel": "travel_enabled",
    "activity": "activity_enabled",
    "keepalive": "keepalive_enabled",
    "school": "school_enabled",
    "cat": "cat_enabled",
    "redeem": "redeem_enabled",
    "lottery": "lottery_enabled",
    "makeup": "makeup_enabled",
}


def gateway_post(path, timeout=30):
    """带 api_key 向网关发 POST。返回 (ok, payload)。"""
    try:
        management_socket = key_management.socket_path(CONFIG_PATH, BASE)
        if management_socket:
            code, payload = key_management.request(management_socket, "POST", path, timeout=timeout)
            return code < 400, payload
    except (OSError, ValueError):
        return False, {"ok": False, "message": "无法读取网关配置"}
    key = api_key()
    req = Request(GATEWAY + path, data=b"", method="POST",
                  headers={"Authorization": "Bearer " + key})
    try:
        with urlopen(req, timeout=timeout) as r:
            return True, json.loads(r.read().decode("utf-8"))
    except HTTPError as e:
        try:
            return False, json.loads(e.read().decode("utf-8"))
        except Exception:
            return False, {"error": {"message": "网关返回 %d" % e.code}}
    except (URLError, OSError, ValueError) as ex:
        return False, {"error": {"message": "连不上网关：%s" % ex}}


def set_task_enabled(key, enabled):
    """改 config.json 的 schedule.<key>_enabled。返回 (ok, message)。

    容器把 config.json 以只读挂进去，所以改完必须重启才生效——由调用方负责重启。
    保留原文件的缩进与键序（json.load 保持插入顺序），只在目标键上动刀。
    """
    field = TASK_ENABLE_KEY.get(key)
    if not field:
        return False, "未知任务：%s" % key
    try:
        with open(CONFIG_PATH, "r", encoding="utf-8") as f:
            cfg = json.load(f)
    except Exception as ex:
        return False, "读取 config.json 失败：%s" % ex
    sch = cfg.get("schedule")
    if not isinstance(sch, dict):
        sch = {}
        cfg["schedule"] = sch
    if sch.get(field) is enabled:
        return True, "状态未变化"
    sch[field] = bool(enabled)
    try:
        tmp = CONFIG_PATH + ".tmp"
        # config.json 含调用密钥：重写后保持原有权限与属主，避免变成 world-readable。
        try:
            original = os.stat(CONFIG_PATH)
        except OSError:
            original = None
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(cfg, f, indent=2, ensure_ascii=False)
            f.write(chr(10))
        if original is not None:
            try:
                os.chmod(tmp, original.st_mode & 0o777)
                os.chown(tmp, original.st_uid, original.st_gid)
            except OSError:
                pass
        os.replace(tmp, CONFIG_PATH)
    except Exception as ex:
        return False, "写入 config.json 失败：%s" % ex
    return True, "已保存（重启后生效）"


def chown_app(path):
    """把网关侧文件归属到运行网关的 uid/gid（10001）并收紧权限。

    失败不再静默：写文件路径上先 chown 再 rename，落地瞬间就应是网关可读的形态；
    这里若失败，说明容器能力或挂载卷有变，账号会加载不到，必须留下痕迹而不是
    让「加号成功但号没生效」再次无解释地发生。
    """
    try:
        shutil.chown(path, user=10001, group=10001)
    except Exception as exc:  # noqa: BLE001 - 记录后继续，不阻断调用方
        sys.stderr.write("chown %s failed: %s\n" % (path, exc))
    try:
        os.chmod(path, 0o600)
    except Exception as exc:  # noqa: BLE001
        sys.stderr.write("chmod %s failed: %s\n" % (path, exc))


def list_auth_files():
    items = []
    if not os.path.isdir(AUTHS_DIR):
        return items
    for name in sorted(os.listdir(AUTHS_DIR)):
        m = AUTH_FILE_RE.match(name)
        if not m:
            continue
        items.append({
            "uid": m.group("uid"),
            "file": name,
            "disabled": bool(m.group("disabled")),
            "path": os.path.join(AUTHS_DIR, name),
        })
    return items


def read_auth(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return None


def auth_summary(entry):
    raw = read_auth(entry["path"]) or {}
    acct = raw.get("account") or {}
    auth = raw.get("auth") or {}
    exp = auth.get("expiresAt") or 0
    try:
        exp = int(exp)
    except (TypeError, ValueError):
        exp = 0
    return {
        "uid": entry["uid"],
        "nickname": acct.get("nickname") or acct.get("uid") or entry["uid"][:8],
        "enterpriseId": acct.get("enterpriseId") or "",
        "realm": auth.get("realm") or "",
        "domain": auth.get("domain") or "",
        "expiresAt": exp,
        "disabled": entry["disabled"],
        "parsable": bool(raw),
    }


def write_auth_file(poll_result):
    uid = poll_result.get("uid") or ""
    if not UID_RE.match(uid):
        raise RuntimeError("上游返回的 uid 不合法: %r" % uid)
    try:
        exp_in = int(poll_result.get("expires_in") or 0)
    except (TypeError, ValueError):
        exp_in = 0

    doc = {
        "account": {
            "uid": uid,
            "enterpriseId": poll_result.get("enterprise_id") or "",
            "nickname": poll_result.get("nickname") or "",
        },
        "auth": {
            "accessToken": poll_result.get("access_token") or "",
            "refreshToken": poll_result.get("refresh_token") or "",
            "expiresAt": int(time.time()) + exp_in,
            "domain": poll_result.get("domain") or "",
            "realm": poll_result.get("realm") or "cn",
        },
    }
    if not doc["auth"]["accessToken"]:
        raise RuntimeError("上游没有返回 access_token")

    os.makedirs(AUTHS_DIR, exist_ok=True)
    final_path = os.path.join(AUTHS_DIR, "workbuddy-%s.json" % uid)
    stale = final_path + ".disabled"
    descriptor, tmp = tempfile.mkstemp(prefix=".workbuddy-", dir=AUTHS_DIR)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as f:
            json.dump(doc, f, indent=1)
            f.flush()
            os.fsync(f.fileno())
        # 先改属主与权限，再原子替换：文件一落地就是网关可读的形态。
        # 反过来（先 replace 再 chown）会留下一个窗口，期间文件属 root:0600，
        # 网关以 uid 10001 运行读不到，会静默跳过该账号（实测只加载 23 个）。
        chown_app(tmp)
        os.replace(tmp, final_path)
        if os.path.exists(stale):
            os.remove(stale)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)

    return {
        "uid": uid,
        "nickname": doc["account"]["nickname"],
        "realm": doc["auth"]["realm"],
        "expiresAt": doc["auth"]["expiresAt"],
        "expiresInDays": round(exp_in / 86400.0, 1),
    }


def find_entry(uid):
    if not UID_RE.match(uid or ""):
        return None
    for e in list_auth_files():
        if e["uid"] == uid:
            return e
    return None


def restart_container():
    t0 = time.monotonic()
    rc, out, err = docker(["restart", CONTAINER], timeout=210)
    if rc != 0:
        return False, "重启失败: %s" % (err or out), 0.0
    if os.environ.get("WB2API_RUNTIME") == "native":
        return True, "配置与账号已重新加载", round(time.monotonic() - t0, 1)
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        time.sleep(1)
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        h = gateway_get("/healthz", timeout=min(5, remaining))
        if isinstance(h, dict) and h.get("service") == "workbuddy2api" and "error" not in h:
            return True, "已重启并加载账号", round(time.monotonic() - t0, 1)
    return False, "容器重启已执行，但网关健康检查未通过，请查看日志后刷新", round(time.monotonic() - t0, 1)


def get_credits(force=False):
    """普通读取不等待；强制刷新等待共享查询，失败后自动读取遵守重试间隔。"""
    with _lock:
        now = time.monotonic()
        if _credit_cache["error"]:
            due = now >= _credit_cache["retry_at"]
        else:
            due = _credit_cache["data"] is None or now - _credit_cache["ts"] >= CREDIT_TTL
        refresh = _credit_refreshing
        if force or due:
            refresh = _start_credit_refresh_locked()
        if not force:
            return _credit_snapshot_locked()
    # 等待期间不占缓存锁；结果属于这一次查询，不会读到随后另一次查询的状态。
    refresh["done"].wait()
    return dict(refresh["result"])


def _credit_snapshot_locked():
    result = dict(_credit_cache["data"] or {"accounts": []})
    result["pending"] = _credit_refreshing is not None
    if _credit_cache["error"]:
        result["error"] = _credit_cache["error"]
    return result


def _finish_credit_refresh_locked(refresh, data):
    global _credit_refreshing
    if _credit_refreshing is refresh:
        now = time.monotonic()
        if data.get("error"):
            _credit_cache["error"] = data["error"]
            _credit_cache["retry_at"] = now + CREDIT_RETRY_INTERVAL
        else:
            _credit_cache.update(ts=now, data=data, error=None, retry_at=0.0)
        _credit_refreshing = None
    refresh["result"] = _credit_snapshot_locked()
    refresh["result"]["pending"] = False
    refresh["done"].set()


def _start_credit_refresh_locked():
    """调用方持有 _lock；普通读取和手动刷新共用查询及完成结果。"""
    global _credit_refreshing
    if _credit_refreshing is not None:
        return _credit_refreshing
    refresh = {"done": threading.Event(), "result": None}
    _credit_refreshing = refresh

    def worker():
        try:
            data = _query_credits()
        except Exception as ex:  # noqa: BLE001 - 异常也要结束等待并回传查询错误
            data = {"error": "积分查询失败：%s" % ex}
        with _lock:
            _finish_credit_refresh_locked(refresh, data)

    try:
        threading.Thread(target=worker, name="credit-refresh", daemon=True).start()
    except Exception as ex:  # noqa: BLE001 - 未能启动线程时不得保留永久 pending
        _finish_credit_refresh_locked(refresh, {"error": "积分查询无法启动：%s" % ex})
    return refresh


def _query_credits():
    """只执行及校验查询；缓存统一由刷新协调器提交，错误不覆盖成功数据。"""
    if not container_running():
        return {"error": "容器未运行"}
    rc, out, err = docker(["exec", CONTAINER, "./credit"], timeout=90)
    if rc != 0:
        return {"error": err or out or "credit 执行失败"}
    try:
        data = json.loads(out)
    except ValueError:
        return {"error": "credit 输出无法解析", "raw": out[:400]}
    if (not isinstance(data, dict) or not isinstance(data.get("accounts"), list)
            or any(not isinstance(account, dict) for account in data["accounts"])):
        return {"error": "credit 输出格式无效"}
    return data


def credits_by_uid(force=False):
    data = get_credits(force=force)
    if not isinstance(data, dict) or "accounts" not in data:
        return {}, data
    return {a.get("uid"): a for a in (data.get("accounts") or [])}, data


def build_state(force_credit=False):
    pool = gateway_get("/status") or {}
    pool_by_uid = {a.get("uid"): a for a in (pool.get("accounts") or [])}
    cred_by_uid, credit_raw = credits_by_uid(force=force_credit)
    # 积分还在后台查（或首次加载尚未拿到）时告诉前端：稍后自己重取一次，
    # 而不是让首屏一直等 `./credit`（冷跑 8 秒）。
    credit_pending = bool(isinstance(credit_raw, dict) and credit_raw.get("pending"))

    accounts = []
    for e in list_auth_files():
        s = auth_summary(e)
        p = pool_by_uid.get(s["uid"]) or {}
        c = cred_by_uid.get(s["uid"]) or {}
        s["pool"] = {
            "inPool": bool(p),
            "healthy": bool(p) and (not p.get("cooling")) and (not p.get("disabled")),
            "cooling": bool(p.get("cooling")),
            "until": p.get("until"),
            "disabled": bool(p.get("disabled")),
            "disabledReason": p.get("disabled_reason") or "",
            "inFlight": p.get("in_flight") or 0,
            "breakerFails": p.get("breaker_fails") or 0,
            "lastSuccess": p.get("last_success"),
            "lastErr": p.get("last_err"),
            # 冷却台账：面板据此显示「为什么冷却、还剩多久」，而不是把凭证有效期
            # 的「剩 N 天」摆在「冷却中」旁边（用户反馈：看起来像冷却要等 363 天）。
            "coolRemaining": p.get("cool_remaining_sec") or 0,
            "coolKind": p.get("cool_kind") or "",
            "reason": p.get("reason") or "",
            "successCount": p.get("success_count") or 0,
            "errTotal": p.get("err_total") or 0,
        }
        s["credits"] = {
            "remain": c.get("remain"),
            "size": c.get("size"),
            "used": c.get("used"),
            "packages": c.get("packages"),
            "ok": c.get("ok"),
        }
        accounts.append(s)

    changed = [a["uid"] for a in accounts if a["disabled"] and a["pool"]["inPool"]]

    return {
        "service": "wb2api-admin",
        "version": PANEL_VERSION,
        "containerRunning": container_running(),
        "accounts": accounts,
        "pool": {
            "total": pool.get("total"),
            "healthy": pool.get("healthy"),
            "cooling": pool.get("cooling"),
            "disabled": pool.get("disabled"),
            "inFlightFull": pool.get("in_flight_full"),
            "realmTotals": pool.get("realm_totals"),
            "stickySessions": pool.get("sticky_sessions"),
        },
        "creditTotals": (credit_raw or {}).get("total") if isinstance(credit_raw, dict) else None,
        "creditError": (credit_raw or {}).get("error") if isinstance(credit_raw, dict) else None,
        "creditPending": credit_pending,
        "pendingRestart": bool(changed),
        "apiKey": "",
        "apiKeysManaged": bool(key_management.socket_path(CONFIG_PATH, BASE)),
    }


class Handler(BaseHTTPRequestHandler):
    server_version = "wb2api-admin/" + PANEL_VERSION
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s - %s" % (self.address_string(), fmt % args) + chr(10))

    def _send(self, code, body, ctype, extra=None):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Referrer-Policy", "same-origin")
        self.send_header("X-Frame-Options", "SAMEORIGIN")
        self.send_header("Content-Security-Policy", "frame-ancestors 'self'; base-uri 'self'; form-action 'self'")
        pending = list(getattr(self, "_pending", []) or [])
        self._pending = []
        for name, value in list(extra or []) + pending:
            self.send_header(name, value)
        self.end_headers()
        if self.command != "HEAD":
            try:
                self.wfile.write(body)
            except (ConnectionError, TimeoutError):
                self.close_connection = True

    def _json(self, code, payload, extra=None):
        self._send(code, json.dumps(payload, ensure_ascii=False).encode("utf-8"),
                   "application/json; charset=utf-8", extra)

    def _html(self, code, text):
        self._send(code, text.encode("utf-8"), "text/html; charset=utf-8")

    def _reject_unread_body(self, code, payload):
        self.close_connection = True
        self._json(code, payload, extra=[("Connection", "close")])
        lengths = self.headers.get_all("Content-Length", [])
        if self.headers.get("Transfer-Encoding") is not None or len(lengths) != 1:
            return
        value = lengths[0].strip()
        if not re.fullmatch(r"[0-9]{1,10}", value):
            return
        remaining = min(int(value), 65536)
        previous_timeout = self.connection.gettimeout()
        deadline = time.monotonic() + 0.1
        try:
            while remaining:
                wait = deadline - time.monotonic()
                if wait <= 0:
                    break
                self.connection.settimeout(wait)
                chunk = self.rfile.read1(min(remaining, 4096))
                if not chunk:
                    break
                remaining -= len(chunk)
        except (OSError, ValueError):
            pass
        finally:
            self.connection.settimeout(previous_timeout)

    def _redirect(self, target):
        self._send(302, b"", "text/plain; charset=utf-8",
                   [("Location", target)])

    def _body(self, limit=65536):
        if self.headers.get("Transfer-Encoding"):
            raise RequestBodyError(400, "不支持此请求传输方式")
        if self.headers.get("Content-Type", "").split(";", 1)[0].strip().lower() != "application/json":
            raise RequestBodyError(415, "请使用 JSON 格式提交")
        lengths = self.headers.get_all("Content-Length", [])
        if len(lengths) != 1 or not lengths[0].isdigit():
            raise RequestBodyError(400, "请求长度无效")
        n = int(lengths[0])
        if n <= 0:
            raise RequestBodyError(400, "请求体不能为空")
        if n > limit:
            raise RequestBodyError(413, "请求体超过大小限制")
        try:
            self.connection.settimeout(10)
            raw = self.rfile.read(n)
            if len(raw) != n:
                raise ValueError("incomplete request")
            body = json.loads(raw.decode("utf-8"))
        except (OSError, ValueError) as error:
            raise RequestBodyError(400, "请求体不是完整的 JSON") from error
        if not isinstance(body, dict):
            raise RequestBodyError(400, "请求体必须是 JSON 对象")
        return body

    # ── 会话 ───────────────────────────────────────────────────────────
    def _session(self, renew=True):
        """从 Cookie 里取会话；顺带把临近过期的会话续期（滑动过期）。"""
        raw = self.headers.get("Cookie") or ""
        if not raw:
            return None
        cookie = SimpleCookie()
        try:
            cookie.load(raw)
        except Exception:
            return None
        morsel = cookie.get(COOKIE_NAME)
        if not morsel:
            return None
        credentials = load_credentials()
        payload = read_session(morsel.value, credentials=credentials)
        if not payload:
            return None
        remaining = int(payload.get("e") or 0) - time.time()
        if renew and remaining < SESSION_TTL / 3.0:
            token, _ = issue_session(payload["u"], nonce=payload["n"], credentials=credentials)
            self._pending = self._cookie_headers(token, SESSION_TTL)
        return payload

    def _need_session(self):
        payload = self._session()
        if not payload:
            return None
        return payload

    def _cookie_headers(self, token, max_age):
        value = ("%s=%s; Path=/; HttpOnly; SameSite=Lax; Max-Age=%d"
                 % (COOKIE_NAME, token, max_age))
        if self.headers.get("X-Forwarded-Proto", "").lower() == "https":
            value += "; Secure"
        return [("Set-Cookie", value), ("Set-Cookie", "%s=; Path=/admin/; HttpOnly; SameSite=Lax; Max-Age=0" % COOKIE_NAME)]

    def _clear_cookie(self):
        return [("Set-Cookie", "%s=; Path=%s; HttpOnly; SameSite=Lax; Max-Age=0" % (COOKIE_NAME, path))
                for path in ("/", "/admin/")]

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/__health" and os.environ.get("WB2API_RUNTIME") == "native":
            return self._json(200, {"ok": True, "version": PANEL_VERSION})
        query = self.path.split("?", 1)[1] if "?" in self.path else ""
        session = self._session()

        if path == "/login":
            if session:
                return self._redirect("./")
            try:
                with open(LOGIN_PATH, "r", encoding="utf-8") as fh:
                    return self._html(200, fh.read())
            except OSError as ex:
                return self._html(500, "登录页缺失: %s" % ex)

        if not session:
            if path.startswith("/api/"):
                return self._json(401, {"ok": False, "error": "unauthorized",
                                        "message": "登录已失效，请重新登录"})
            return self._redirect("login")

        if path in ("/", "/index.html"):
            try:
                with open(INDEX_PATH, "r", encoding="utf-8") as f:
                    return self._html(200, f.read())
            except Exception as ex:
                return self._html(500, "面板前端缺失: %s" % ex)

        if path.startswith("/vendor/"):
            fp = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                             "vendor", os.path.basename(path))
            if not os.path.isfile(fp):
                return self._json(404, {"error": "not found"})
            with open(fp, "rb") as fh:
                return self._send(200, fh.read(), "application/javascript; charset=utf-8")

        if path == "/api/state":
            force = "refresh_credit=1" in query
            try:
                return self._json(200, build_state(force_credit=force))
            except Exception as ex:
                return self._json(500, {"error": str(ex)})

        if path == "/api/session":
            doc = load_credentials()
            return self._json(200, {
                "ok": True,
                "username": doc.get("username", ""),
                "expiresAt": session.get("e"),
                "ttl": SESSION_TTL,
                "inherited": bool(doc.get("inherited")),
            })

        if path == "/api/keys":
            return self.keys_with_usage()

        if path == "/api/key-limits" or path == "/api/requests" or path.startswith("/api/requests/"):
            return self.ops_get(path, query)

        if path == "/api/usage":
            # 用量账本只经本机管理通道读取：面板能看到全量，普通调用密钥看不到。
            return self._gateway_admin("GET", "/usage")

        if path == "/api/update":
            return self._gateway_admin("GET", "/update")

        if path == "/api/features/reasoning-loop":
            # 重复推理保护的「命中后怎么办」：网关内部接口只走本机管理通道。
            return self._gateway_admin("GET", "/features/reasoning-loop")

        if path == "/api/models":
            # 面板经本机管理通道读取完整模型列表，不受单个调用密钥的绑定限制。
            payload = gateway_get("/v1/models")
            ids = []
            if isinstance(payload, dict):
                for item in payload.get("data") or []:
                    if isinstance(item, dict) and isinstance(item.get("id"), str) and item["id"]:
                        ids.append(item["id"])
            return self._json(200, {"ok": True, "models": sorted(set(ids))})

        if path == "/api/tasks":
            payload = gateway_get("/tasks")
            if payload is not None:
                return self._json(200, payload)
            return self._json(200, {"available": False, "tasks": [],
                                    "error": "连不上网关或网关未接入排程"})

        if path == "/api/task/log":
            mk = re.search(r"key=([a-z_]+)", query)
            key = mk.group(1) if mk else ""
            if key not in TASK_ENABLE_KEY:
                return self._json(200, {"ok": False, "lines": [], "message": "未知任务：%s" % key})
            payload = gateway_get("/tasks/%s/log" % key)
            if payload is None:
                return self._json(200, {"ok": False, "lines": [], "message": "连不上网关或网关未接入排程"})
            lines = payload.get("lines") or []
            return self._json(200, {"ok": True, "lines": lines, "count": len(lines),
                                    "message": (payload.get("error") or {}).get("message", "")})

        if path == "/api/logs":
            # 这里收到的 lines= 是「要多少条**请求**」，不是 docker --tail 的原始行数。
            # 日志里请求行与 WARN/ERR 行混在一起，比例不固定，所以先多取一段原始行，
            # 解析后按请求行裁到目标条数；日志本身不够长时就给多少算多少。
            want = 120
            m = re.search(r"lines=(\d{1,4})", query)
            if m:
                want = min(max(int(m.group(1)), 1), 1000)
            rc, out, err = docker(["logs", "--tail", str(request_window_size(want)), CONTAINER], timeout=40)
            raw = "\n".join(part for part in (out, err) if part)
            rows, other = parse_request_log(raw)
            rows = rows[-want:]
            return self._json(200, {"ok": rc == 0, "logs": raw, "rows": rows, "other": other,
                                    "count": len(rows), "rc": rc})

        return self._json(404, {"error": "not found"})

    def do_POST(self):
        path = self.path.split("?")[0]
        if path in ("/api/keys", "/api/keys/update", "/api/keys/delete", "/api/keys/copy"):
            return self.keys_post(path)
        try:
            if not self._origin_ok() or self.headers.get("X-Admin-Request") != "1":
                return self._reject_unread_body(403, {"ok": False, "message": "请求来源无效，请从管理页面重新操作"})
            body = self._body(8192 if path.startswith("/api/update/") else 65536)
            self._validate_action_body(path, body)
            if path == "/api/auth/login":
                return self.auth_login(body)
            if path == "/api/auth/logout":
                return self.auth_logout()

            session = self._session()
            if not session:
                return self._json(401, {"ok": False, "error": "unauthorized",
                                        "message": "登录已失效，请重新登录"})

            if path == "/api/auth/password":
                return self.auth_password(body, session)
            if path == "/api/login/start":
                return self._json(200, self.login_start(body))
            if path == "/api/login/poll":
                return self._json(200, self.login_poll(body))
            if path == "/api/account/toggle":
                return self._json(200, self.account_toggle(body))
            if path == "/api/account/delete":
                return self._json(200, self.account_delete(body))
            if path == "/api/task/run":
                return self._json(200, self.task_run(body))
            if path == "/api/task/toggle":
                return self._json(200, self.task_toggle(body))
            if path == "/api/service/restart":
                return self._json(200, self.service_restart())
            if path == "/api/features/reasoning-loop":
                return self._json(200, self.reasoning_loop_toggle(body))
            if path in ("/api/update/check", "/api/update/apply"):
                return self.update_post(path, body)
            if path == "/api/credit":
                return self._json(200, {"credit": get_credits(force=True)})
        except RequestBodyError as ex:
            self.close_connection = True
            return self._json(ex.status, {"ok": False, "message": str(ex)})
        except Exception as ex:
            return self._json(500, {"ok": False, "message": str(ex)})
        return self._json(404, {"error": "not found"})

    def _validate_action_body(self, path, body):
        shapes = {
            "/api/auth/login": {"username", "password"},
            "/api/auth/logout": set(),
            "/api/auth/password": {"current", "username", "password", "confirm"},
            "/api/login/start": {"realm"}, "/api/login/poll": {"realm", "login_id"},
            "/api/account/toggle": {"uid", "disabled"}, "/api/account/delete": {"uid"},
            "/api/task/run": {"key"}, "/api/task/toggle": {"key", "enabled"},
            "/api/service/restart": set(), "/api/credit": set(),
            "/api/features/reasoning-loop": {"stop_only"},
        }
        allowed = shapes.get(path)
        if allowed is not None and set(body) - allowed:
            raise RequestBodyError(400, "包含不支持的字段")
        for field, value in body.items():
            if field in {"username", "password", "current", "confirm", "realm", "uid", "key"} and not isinstance(value, str):
                raise RequestBodyError(400, "字段格式不正确：" + field)
        if "login_id" in body and (not isinstance(body["login_id"], str) or not re.fullmatch(r"[a-f0-9]{32}", body["login_id"])):
            raise RequestBodyError(400, "登录流程标识无效，请重新生成链接")
        for route, field in (("/api/account/toggle", "disabled"), ("/api/task/toggle", "enabled")):
            if path == route and type(body.get(field)) is not bool:
                raise RequestBodyError(400, "开关值必须明确指定为 true 或 false")
        if path == "/api/features/reasoning-loop" and type(body.get("stop_only")) is not bool:
            raise RequestBodyError(400, "开关值必须明确指定为 true 或 false")
        if path.startswith("/api/account/") and not UID_RE.fullmatch(body.get("uid", "")):
            raise RequestBodyError(400, "账号标识不正确")
        if path in ("/api/task/run", "/api/task/toggle") and body.get("key") not in TASK_ENABLE_KEY:
            raise RequestBodyError(400, "任务标识不正确")

    def keys_request(self, method, endpoint, body=None):
        try:
            path = key_management.socket_path(CONFIG_PATH, BASE)
        except (OSError, ValueError):
            return self._json(503, {"ok": False, "message": "无法读取密钥管理配置"})
        code, result = key_management.request(path, method, endpoint, body)
        return self._json(code, result)

    def keys_with_usage(self):
        """密钥列表附带每把密钥的累计 token 用量。

        用量账本与密钥库是两个独立文件，面板在这里做一次合并，省得前端再拉一次
        /api/usage 并按 key_id 对齐。账本不可用时只丢用量列，密钥列表照常返回。
        """
        try:
            socket = key_management.socket_path(CONFIG_PATH, BASE)
        except (OSError, ValueError):
            return self._json(503, {"ok": False, "message": "无法读取密钥管理配置"})
        code, result = key_management.request(socket, "GET", "/keys")
        if code != 200 or not isinstance(result, dict) or not isinstance(result.get("keys"), list):
            return self._json(code, result)
        totals = {}
        usage_code, usage = key_management.request(socket, "GET", "/usage")
        usage_available = (usage_code == 200 and isinstance(usage, dict) and usage.get("ok") is True
                           and isinstance(usage.get("keys"), list))
        if usage_available:
            for item in usage.get("keys") or []:
                if isinstance(item, dict) and isinstance(item.get("key_id"), str):
                    totals[item["key_id"]] = (item.get("totals") or {}).get("total_tokens") or 0
        for entry in result["keys"]:
            if isinstance(entry, dict):
                entry["total_tokens"] = totals.get(entry.get("id"), 0) if usage_available else None
        result["usage_available"] = usage_available
        return self._json(200, result)

    def ops_get(self, path, query):
        """管理员只读通道：构造固定端点，保留 400/404/503 与筛选参数含义。"""
        if len(query) > 2048:
            return self._json(400, {"ok": False, "message": "筛选条件过长"})
        if path == "/api/key-limits":
            if query:
                return self._json(400, {"ok": False, "message": "限流状态接口不接受筛选参数"})
            return self.keys_request("GET", "/key-limits")
        if path.startswith("/api/requests/"):
            try:
                request_id = unquote(path[len("/api/requests/"):], errors="strict")
            except UnicodeError:
                request_id = ""
            if query or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", request_id):
                return self._json(400, {"ok": False, "message": "请求标识不正确"})
            return self.keys_request("GET", "/requests/" + request_id)
        try:
            pairs = parse_qsl(query, keep_blank_values=True, strict_parsing=True, errors="strict", max_num_fields=6) if query else []
        except (ValueError, UnicodeError):
            return self._json(400, {"ok": False, "message": "筛选参数格式错误"})
        allowed = {"key_id", "model", "status", "request_id", "offset", "limit"}
        values = dict(pairs)
        if len(values) != len(pairs) or set(values) - allowed:
            return self._json(400, {"ok": False, "message": "包含重复或不支持的筛选参数"})
        for name, value in pairs:
            if len(value) > (256 if name == "model" else 128) or any(ord(c) < 32 or ord(c) == 127 for c in value):
                return self._json(400, {"ok": False, "message": "筛选值无效"})
            if name in ("offset", "limit") and (not re.fullmatch(r"[0-9]{1,9}", value) or (name == "limit" and not 1 <= int(value) <= 100)):
                return self._json(400, {"ok": False, "message": "分页参数无效"})
        if values.get("status", "") not in ("", "success", "error", "canceled", "rejected"):
            return self._json(400, {"ok": False, "message": "请求状态无效"})
        return self.keys_request("GET", "/requests" + ("?" + urlencode(pairs) if pairs else ""))

    def _origin_ok(self):
        """同源校验：没带 Origin（同源表单/脚本）或与 Host 完全一致才算合法。"""
        origin = self.headers.get("Origin")
        try:
            parsed = urlsplit(origin) if origin else None
        except ValueError:
            return False
        if not parsed:
            return True
        return (parsed.scheme in ("http", "https")
                and parsed.netloc.lower() == self.headers.get("Host", "").lower()
                and not parsed.username)

    def _gateway_admin(self, method, endpoint, body=None):
        """经本机管理 socket 调用网关内部接口（用量、热更新等），不暴露给调用密钥。"""
        try:
            socket = key_management.socket_path(CONFIG_PATH, BASE)
        except (OSError, ValueError):
            return self._json(200, {"ok": False, "message": "无法读取网关配置"})
        code, result = key_management.request(socket, method, endpoint, body)
        if code != 200 or not isinstance(result, dict):
            message = result.get("message") if isinstance(result, dict) else ""
            return self._json(200, {"ok": False,
                                    "message": message or "网关未响应（旧版本网关请先升级）"})
        return self._json(200, result)

    def update_post(self, path, body):
        """热更新操作：登录后由同源管理页面触发，闸门与密钥写操作一致。"""
        if not self._session():
            return self._json(401, {"ok": False, "message": "请先登录管理面板"})
        if not self._origin_ok() or self.headers.get("X-Admin-Request") != "1":
            return self._json(403, {"ok": False, "message": "请求来源无效，请从管理页面重新操作"})
        if not isinstance(body, dict) or set(body) - {"tag"}:
            return self._json(400, {"ok": False, "message": "包含不支持的字段"})
        tag = body.get("tag")
        if tag is not None and (not isinstance(tag, str) or len(tag) > 64 or not re.fullmatch(r"[A-Za-z0-9._-]*", tag)):
            return self._json(400, {"ok": False, "message": "版本号格式不正确"})
        endpoint = "/update/check" if path.endswith("/check") else "/update/apply"
        payload = {"tag": tag} if tag else {}
        return self._gateway_admin("POST", endpoint, payload)

    def keys_post(self, path):
        if not self._session():
            return self._reject_unread_body(401, {"ok": False, "message": "请先登录管理面板"})
        if not self._origin_ok() or self.headers.get("X-Admin-Request") != "1":
            return self._reject_unread_body(403, {"ok": False, "message": "请求来源无效，请从管理页面重新操作"})
        try:
            body = self._body(8192)
        except RequestBodyError as error:
            self.close_connection = True
            return self._json(error.status, {"ok": False, "message": str(error)})
        if "reasoning_loop_guard" in body and type(body["reasoning_loop_guard"]) is not bool:
            return self._json(400, {"ok": False, "message": "重复推理保护必须为开启或关闭"})
        if "global_fallback_to_cn" in body and type(body["global_fallback_to_cn"]) is not bool:
            return self._json(400, {"ok": False, "message": "CN 回落必须为开启或关闭"})
        if "limits" in body and not self._valid_limits(body["limits"]):
            return self._json(400, {"ok": False, "message": "限流需为整数：每分钟 0–60000 次、并发 0–256、等待 0–30 秒；排队需先设置并发上限"})
        if path == "/api/keys":
            if set(body) - {"name", "note", "models", "reasoning_loop_guard",
                            "global_fallback_to_cn", "expires_at", "limits"}:
                return self._json(400, {"ok": False, "message": "包含不支持的字段"})
            if "expires_at" in body and not self._valid_expiry(body["expires_at"]):
                return self._json(400, {"ok": False, "message": "有效期需为 RFC3339 时间，留空表示无限制"})
            problem = self._model_binding_error(body.get("models"))
            if problem:
                return self._json(400, {"ok": False, "message": problem})
            return self.keys_request("POST", "/keys", body)
        key_id = body.get("id")
        if not isinstance(key_id, str) or not re.fullmatch(r"legacy|key_[0-9a-f]{24}", key_id):
            return self._json(400, {"ok": False, "message": "密钥标识不正确"})
        if path == "/api/keys/copy":
            if set(body) != {"id"}:
                return self._json(400, {"ok": False, "message": "包含不支持的字段"})
            return self.keys_request("POST", "/keys/" + key_id + "/copy", {})
        if path == "/api/keys/delete":
            if set(body) != {"id"}:
                return self._json(400, {"ok": False, "message": "包含不支持的字段"})
            return self.keys_request("DELETE", "/keys/" + key_id)
        changes = {k: v for k, v in body.items() if k != "id"}
        if not changes or set(changes) - {"name", "note", "enabled", "models", "reasoning_loop_guard",
                                          "global_fallback_to_cn", "expires_at", "limits"}:
            return self._json(400, {"ok": False, "message": "没有有效的修改字段"})
        if "expires_at" in changes and not self._valid_expiry(changes["expires_at"]):
            return self._json(400, {"ok": False, "message": "有效期需为 RFC3339 时间，留空表示无限制"})
        if "models" in changes:
            problem = self._model_binding_error(changes["models"])
            if problem:
                return self._json(400, {"ok": False, "message": problem})
        return self.keys_request("PATCH", "/keys/" + key_id, changes)

    @staticmethod
    def _valid_limits(value):
        if value is None:
            return True
        maximums = {"requests_per_minute": 60000, "max_concurrent": 256, "queue_timeout_seconds": 30}
        if not isinstance(value, dict) or set(value) - set(maximums):
            return False
        if any(type(number) is not int or not 0 <= number <= maximums[name] for name, number in value.items()):
            return False
        return value.get("queue_timeout_seconds", 0) == 0 or value.get("max_concurrent", 0) > 0

    @staticmethod
    def _model_binding_error(models):
        """校验模型绑定，返回错误文案；None 表示通过。

        绑定按完整模型名逐字比对，裸名一条也匹配不上——模型列表里只有带
        cn:/global: 前缀的名字。网关侧也会拒裸名，这里先拦一道，是为了让
        手填的场景在表单上就得到明确提示，而不是保存成功却发现调用全 403。
        """
        if models is None:
            return None
        if not isinstance(models, list) or len(models) > 64 or any(
                not isinstance(item, str) or not item or len(item) > 64 or
                item.strip() != item or any(ch.isspace() or ord(ch) < 32 for ch in item)
                for item in models) or len(set(models)) != len(models):
            return "模型绑定需为最多 64 个不重复的模型名"
        # 前缀后必须还有模型名：`cn:` / `global:` 这种只有前缀的写法同样匹配不上。
        if any(not re.fullmatch(r"(cn|global):.+", item) for item in models):
            return "模型绑定必须选完整模型名（cn: 或 global: 开头），请从模型列表添加"
        return None

    @staticmethod
    def _valid_expiry(value):
        """有效期允许 null（无限制）或 RFC3339 字符串；范围与格式由网关再校验一次。"""
        if value is None:
            return True
        return isinstance(value, str) and len(value) <= 40

    # ── 登录 / 改密 ────────────────────────────────────────────────────
    def auth_login(self, body):
        ip = _client_ip(self)
        with _cred_lock:
            blocked, hits = login_blocked(ip)
            if not blocked:
                login_failed(ip)
        if blocked:
            return self._json(429, {
                "ok": False,
                "message": "失败次数过多，请 %d 分钟后再试" % int(LOGIN_WINDOW // 60),
            })

        doc = load_credentials()
        name = str(body.get("username") or "").strip()
        pw = str(body.get("password") or "")
        if not name or not pw:
            return self._json(400, {"ok": False, "message": "请输入用户名和密码"})

        name_ok = hmac.compare_digest(name.encode("utf-8"),
                                      str(doc.get("username", "")).encode("utf-8"))
        if not (name_ok and verify_password(pw, doc.get("password"))):
            left = max(0, LOGIN_MAX_FAILS - (hits + 1))
            return self._json(401, {
                "ok": False,
                "message": "用户名或密码错误" + ("，还可尝试 %d 次" % left if left else ""),
            })

        with _cred_lock:
            unchanged = _same_credentials(doc, load_credentials())
            if unchanged:
                login_ok(ip)
                token, payload = issue_session(doc["username"], credentials=doc)
        if not unchanged:
            return self._json(401, {"ok": False, "message": "登录凭证已更改，请使用新密码重新登录"})
        return self._json(200, {"ok": True, "username": doc["username"],
                                "expiresAt": payload["e"], "ttl": SESSION_TTL},
                          extra=self._cookie_headers(token, SESSION_TTL))

    def auth_logout(self):
        payload = self._session(renew=False)
        self._pending = []
        if payload and payload.get("n"):
            with _cred_lock:
                doc = load_credentials()
                now = time.time()
                revoked = {nonce: expiry for nonce, expiry in doc.get("revokedSessions", {}).items()
                           if isinstance(expiry, (int, float)) and expiry > now}
                revoked[payload["n"]] = max(payload["e"], now + SESSION_TTL)
                if len(revoked) > 4096:
                    doc["sessionKey"] = secrets.token_hex(32)
                    revoked = {}
                doc["revokedSessions"] = revoked
                _save_credentials(doc)
                _revoked.clear()
                _revoked.update(revoked)
        return self._json(200, {"ok": True, "message": "已退出登录"},
                          extra=self._clear_cookie())

    def auth_password(self, body, session):
        doc = load_credentials()
        current = str(body.get("current") or "")
        new_user = str(body.get("username") or "").strip()
        new_pw = str(body.get("password") or "")
        confirm = str(body.get("confirm") or "")

        if not verify_password(current, doc.get("password")):
            return self._json(400, {"ok": False, "message": "当前密码不正确"})
        if len(new_pw) < 8:
            return self._json(400, {"ok": False, "message": "新密码至少 8 位"})
        if new_pw != confirm:
            return self._json(400, {"ok": False, "message": "两次输入的新密码不一致"})
        if new_pw == current:
            return self._json(400, {"ok": False, "message": "新密码不能与当前密码相同"})
        if not re.match(r"^[A-Za-z0-9_.-]{3,32}$", new_user or ""):
            return self._json(400, {
                "ok": False,
                "message": "用户名需为 3-32 位字母、数字、下划线、点或连字符",
            })

        replacement_hash = hash_password(new_pw)
        save_error = None
        with _cred_lock:
            unchanged = _same_credentials(doc, load_credentials())
            if unchanged:
                history = list(doc.get("history") or [])
                history.append({"password": doc.get("password"), "at": int(time.time())})
                doc = {
                    "version": 1,
                    "username": new_user,
                    "password": replacement_hash,
                    "sessionKey": secrets.token_hex(32),
                    "updatedAt": int(time.time()),
                    "history": history[-5:],
                    "inherited": False,
                }
                try:
                    _save_credentials(doc)
                except OSError as ex:
                    save_error = ex
        if not unchanged:
            return self._json(401, {"ok": False, "message": "登录凭证已更改，请重新登录后修改"})
        if save_error is not None:
            return self._json(500, {"ok": False, "message": "写入失败：%s" % save_error})

        # 换密即换 sessionKey：所有旧会话（包括当前这条）一并失效，强制重登
        token, payload = issue_session(doc["username"], credentials=doc)
        return self._json(200, {
            "ok": True,
            "message": "账号信息已更新，其他设备上的登录已失效",
            "username": doc["username"],
            "expiresAt": payload["e"],
        }, extra=self._cookie_headers(token, SESSION_TTL))

    def login_start(self, body):
        realm = (body.get("realm") or "cn").strip().lower()
        if realm not in REALMS:
            return {"ok": False, "message": "realm 只能是 cn 或 global"}
        if not container_running():
            return {"ok": False, "message": "容器未运行，先启动服务"}
        login_id = secrets.token_hex(16)
        rc, out, err = docker(["exec", CONTAINER, "./login", "--realm=%s" % realm, "--session=" + login_id, "url"],
                              timeout=60)
        if rc != 0:
            return {"ok": False, "message": err or out or "获取授权链接失败"}
        url = out.strip().splitlines()[-1].strip() if out.strip() else ""
        if not url.startswith("http"):
            return {"ok": False, "message": "没拿到授权链接: %s" % (out or err)[:200]}
        return {"ok": True, "url": url, "realm": realm, "login_id": login_id}

    def login_poll(self, body):
        realm = (body.get("realm") or "cn").strip().lower()
        if realm not in REALMS:
            return {"ok": False, "message": "realm 只能是 cn 或 global"}
        login_id = body.get("login_id")
        if not isinstance(login_id, str) or not re.fullmatch(r"[a-f0-9]{32}", login_id):
            return {"ok": False, "message": "登录流程已失效，请重新生成链接"}
        rc, out, err = docker(["exec", CONTAINER, "./login", "--realm=%s" % realm, "--session=" + login_id, "poll"],
                              timeout=90)
        if rc != 0:
            return {"ok": False, "pending": True, "message": err or out or "poll 失败"}
        try:
            result = json.loads(out)
        except ValueError:
            return {"ok": False, "message": "poll 输出无法解析: %s" % out[:200]}

        with _lock:
            summary = write_auth_file(result)
            ok, msg, secs = restart_container()
        if not ok:
            return {"ok": False, "message": "凭证已保存，但 %s" % msg, "account": summary}
        return {"ok": True, "account": summary,
                "message": "账号已添加(%s)，重启耗时 %ss" % (summary["nickname"], secs)}

    def account_toggle(self, body):
        uid = body.get("uid") or ""
        want_disabled = bool(body.get("disabled"))
        with _lock:
            entry = find_entry(uid)
            if not entry:
                return {"ok": False, "message": "找不到账号 %s" % uid}
            if entry["disabled"] == want_disabled:
                return {"ok": True, "message": "状态未变化", "restart": None}
            src = entry["path"]
            dst = src + ".disabled" if want_disabled else src[:-len(".disabled")]
            if os.path.exists(dst):
                return {"ok": False, "message": "存在同名的启用与禁用凭据，已保留两份文件，请先核对重复账号"}
            os.rename(src, dst)
            chown_app(dst)
            ok, msg, secs = restart_container()
        action = "禁用" if want_disabled else "启用"
        return {"ok": ok, "restart": secs if ok else None,
                "message": "已%s %s，%s" % (action, entry["uid"][:8], msg)}

    def account_delete(self, body):
        uid = body.get("uid") or ""
        with _lock:
            entry = find_entry(uid)
            if not entry:
                return {"ok": False, "message": "找不到账号 %s" % uid}
            os.makedirs(TRASH_DIR, mode=0o700, exist_ok=True)
            stamp = time.strftime("%Y%m%d-%H%M%S")
            dst = os.path.join(TRASH_DIR, "%s.%s.%s" % (entry["file"], stamp, secrets.token_hex(6)))
            shutil.move(entry["path"], dst)
            ok, msg, secs = restart_container()
        return {"ok": ok, "restart": secs if ok else None,
                "trashed": os.path.basename(dst),
                "message": "已删除 %s(回收件 %s)，%s" % (
                    entry["uid"][:8], os.path.basename(dst), msg)}

    def task_run(self, body):
        key = (body.get("key") or "").strip()
        if key not in TASK_ENABLE_KEY:
            return {"ok": False, "message": "未知任务：%s" % key}
        ok, payload = gateway_post("/tasks/%s/run" % key)
        if not ok:
            msg = (payload.get("error") or {}).get("message") or "触发失败"
            return {"ok": False, "message": msg}
        return {"ok": True, "message": "已触发，结果可在下方日志里看到"}

    def task_toggle(self, body):
        key = (body.get("key") or "").strip()
        enabled = bool(body.get("enabled"))
        with _lock:
            ok, msg = set_task_enabled(key, enabled)
            if not ok:
                return {"ok": False, "message": msg}
            if msg == "状态未变化":
                return {"ok": True, "message": msg}
            rok, rmsg, secs = restart_container()
        return {"ok": rok, "restart": secs if rok else None,
                "message": "已%s「%s」，%s" % ("启用" if enabled else "停用", key, rmsg if rok else rmsg)}

    def service_restart(self):
        with _lock:
            ok, msg, secs = restart_container()
        return {"ok": ok, "restart": secs if ok else None, "message": msg}

    def reasoning_loop_toggle(self, body):
        """热切换重复推理保护的「命中后怎么办」，经本机管理通道转发给网关。

        与 config.json 里的 features.reasoning_loop_stop_only 是同一个语义：false
        （默认）命中后同账号重发一次，true 命中即停止并如实报错。运行期值优先，
        不需要重启，也不需要改配置文件。
        """
        stop_only = body.get("stop_only")
        code, result = key_management.request(
            key_management.socket_path(CONFIG_PATH, BASE), "POST",
            "/features/reasoning-loop", {"stop_only": stop_only})
        if code != 200 or not isinstance(result, dict) or not result.get("ok"):
            message = result.get("message") if isinstance(result, dict) else ""
            return {"ok": False,
                    "message": message or "网关未响应（旧版本网关请先升级）"}
        return {"ok": True, "stop_only": bool(result.get("stop_only")),
                "message": "已切换为「命中即停止」" if result.get("stop_only")
                           else "已切换为「命中后自动重发」"}


class RequestBodyError(ValueError):
    def __init__(self, status, message):
        super().__init__(message)
        self.status = status


def main():
    os.makedirs(AUTHS_DIR, exist_ok=True)
    socket_path = os.environ.get("WB2API_PANEL_SOCKET")
    if socket_path:
        load_credentials()
        if os.name == "posix" and os.getpgrp() == os.getpid():
            def stop_native_group(_signum, _frame):
                signal.signal(signal.SIGTERM, signal.SIG_IGN)
                os.killpg(os.getpgrp(), signal.SIGTERM)
                raise SystemExit(0)
            signal.signal(signal.SIGTERM, stop_native_group)
        class LocalServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
            daemon_threads = True
            def get_request(self):
                connection, _ = super().get_request()
                connection.settimeout(30)
                return connection, ('127.0.0.1', 0)
        srv = LocalServer(socket_path, Handler)
        os.chmod(socket_path, 0o600)
        sys.stderr.write("embedded administration ready\n")
    else:
        srv = ThreadingHTTPServer((LISTEN_HOST, LISTEN_PORT), Handler)
        sys.stderr.write("wb2api-admin listening on %s:%d" % (LISTEN_HOST, LISTEN_PORT) + chr(10))
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
