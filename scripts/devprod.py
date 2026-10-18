#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""dev → prod 两段式部署工具。

流程（用户 2026-10-18 定：VPS 作 dev，华为云作 prod，先 dev 测试再发版）：

    build    构建 amd64 + arm64 运行包（不发布、不部署）
    deploy   --env dev|prod 部署到指定环境
    verify   --env dev|prod 冒烟：healthz + 三域 servable + 模型清单
    promote  把 dev 已验证的同一份产物部署到 prod（校验产物哈希一致）

凭据只从既有文件读（~/.vps_cred / huawei.env），命令与输出里不出现密码。

为什么要 dev 这一环：v3.10.11 之前 Trae 目录修复连改 6 轮，每轮都直接上生产，
其中 probe1 把 Remote 退路堵死让原本能用的 glm-5.1/qwen-3.5 变成 502。dev 上先跑，
同样的回归就能在发版前拦住。

用法：
    python tools/devprod_20261018.py build --version v3.10.12
    python tools/devprod_20261018.py deploy --env dev --version v3.10.12
    python tools/devprod_20261018.py verify --env dev --version v3.10.12
    python tools/devprod_20261018.py promote --version v3.10.12
"""
import argparse
import datetime
import hashlib
import io
import json
import os
import posixpath
import subprocess
import sys
import tarfile
import time
from pathlib import Path

import paramiko

HERE = Path(__file__).resolve().parent          # gateway/scripts
GATEWAY = HERE.parent                            # gateway
REPO = GATEWAY.parent                            # 仓库根
OUT = REPO / "artifacts" / "devprod-20261018"
GO = r"C:\tools\go\bin\go.exe"

ROOT = "/opt/workbuddy2api"
UPDATES = ROOT + "/data/updates"
CONTAINER = "workbuddy2api"

VPS_CRED = Path.home() / ".vps_cred"
HUAWEI_ENV = REPO.parent / "_probe_channel" / "huawei.env"

HELPERS = (("login", "login"), ("credit", "credit"), ("signin_bin", "signin"),
           ("trial_bin", "trial"), ("activity_bin", "activity"), ("fission_bin", "fission"))
SCRIPTS = ("login.sh", "credit.sh", "signin.sh", "trial.sh", "fission.sh",
           "scripts/global_region.py", "scripts/task_common.py", "scripts/task_runner.py",
           "scripts/school_open_day_2026.py", "scripts/growth_center.py",
           "scripts/compensation_claim.py")

# dev = VPS（aarch64），prod = 华为云（x86_64）
ENVS = {
    "dev": {"label": "dev/VPS", "arch": "arm64", "goarch": "arm64"},
    "prod": {"label": "prod/华为云", "arch": "amd64", "goarch": "amd64"},
}


# ── 连接（凭据只从文件读）───────────────────────────────────────────────────
def load_cred(env):
    if env == "dev":
        with io.open(VPS_CRED, encoding="utf-8") as f:
            c = json.load(f)
        return c["host"], c.get("user", "root"), c["pwd"]
    cfg = {}
    if HUAWEI_ENV.exists():
        with io.open(HUAWEI_ENV, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if line and not line.startswith("#") and "=" in line:
                    k, v = line.split("=", 1)
                    cfg[k.strip()] = v.strip()
    for k in ("HUAWEI_HOST", "HUAWEI_USER", "HUAWEI_PASS"):
        if os.environ.get(k):
            cfg[k] = os.environ[k]
    missing = [k for k in ("HUAWEI_HOST", "HUAWEI_USER", "HUAWEI_PASS") if not cfg.get(k)]
    if missing:
        raise SystemExit("缺配置 %s —— 填 %s" % (", ".join(missing), HUAWEI_ENV))
    return cfg["HUAWEI_HOST"], cfg["HUAWEI_USER"], cfg["HUAWEI_PASS"]


def connect(env):
    host, user, pwd = load_cred(env)
    cli = paramiko.SSHClient()
    cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    cli.connect(host, username=user, password=pwd, timeout=30,
                banner_timeout=30, auth_timeout=30)
    cli.get_transport().set_keepalive(15)
    return cli


def run(cli, cmd, timeout=600):
    _i, o, e = cli.exec_command(cmd, timeout=timeout)
    # 先读完 stdout/stderr 再取退出码：远端输出量大时，不读会让远端写阻塞、
    # 永远不退出（recv_exit_status 提前调用就是死锁）。channel 要单独存，
    # 因为 out 之后会被重绑定成解码后的字符串。
    chan = o.channel
    out = o.read().decode("utf-8", "replace")
    err = e.read().decode("utf-8", "replace")
    return chan.recv_exit_status(), out, err


def push(cli, local, remote, timeout=900):
    size = os.path.getsize(local)
    stdin, stdout, stderr = cli.exec_command("cat > %s" % remote, timeout=timeout)
    sent = 0
    with io.open(local, "rb") as f:
        while True:
            b = f.read(262144)
            if not b:
                break
            stdin.write(b)
            stdin.flush()
            sent += len(b)
    stdin.channel.shutdown_write()
    rc = stdout.channel.recv_exit_status()
    if rc != 0 or sent != size:
        raise RuntimeError("push failed rc=%d sent=%d/%d %s"
                           % (rc, sent, size, stderr.read().decode("utf-8", "replace")[:200]))


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


# ── build ───────────────────────────────────────────────────────────────────
def build(version, commit, arches=(("arm64", "arm64"), ("amd64", "amd64"))):
    """构建运行包。产物落在 OUT/<version>/assets/。

    arches 默认是部署用的两个 64 位目标。要发 Release 给行空板（armv7，
    DFRobot UNIHIKER M10）时传
    (("arm64","arm64"), ("amd64","amd64"), ("armv7","arm"))，
    脚本会自动补 GOARM=7——资产名必须是 wb2api-runtime-linux-armv7.tar.gz，
    与 hotupdate 的 runtimeArchName() 对齐，否则板子找不到本机架构的资产。
    """
    assets = OUT / version / "assets"
    if assets.exists() and any(assets.iterdir()):
        raise SystemExit("产物已存在，先清理：%s" % assets)
    assets.mkdir(parents=True, exist_ok=True)
    built_at = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    # GOTOOLCHAIN 用默认（auto）：go.mod 要求 1.26.8，本机是 1.23.12，
    # local 会直接拒绝。发布工具用的是 go1.26.8 固定值，这里保持一致。
    env_base = dict(os.environ, GOOS="linux", CGO_ENABLED="0", GOMAXPROCS="4",
                    GOTOOLCHAIN="go1.26.8",
                    HTTP_PROXY=os.environ.get("HTTP_PROXY", ""),
                    HTTPS_PROXY=os.environ.get("HTTPS_PROXY", ""))
    for arch, goarch in arches:
        stage = OUT / version / ("runtime-" + arch)
        stage.mkdir(parents=True, exist_ok=True)
        flags = "-s -w -X workbuddy2api/internal/version.Version=%s -X workbuddy2api/internal/version.Commit=%s -X workbuddy2api/internal/version.BuiltAt=%s" % (
            version, commit, built_at)
        env = dict(env_base, GOARCH=goarch)
        # 32 位 ARM 必须显式给 GOARM：Go 的 arm 目标有 v5/v6/v7 三个 ABI，
        # 而 hotupdate 的 goarmVersion() 运行期读的也是 GOARM——两端必须一致，
        # 否则资产名（wb2api-runtime-linux-armv7.tar.gz）对不上。
        if goarch == "arm":
            env["GOARM"] = "7"
        targets = [("wb2api", "./cmd/server", flags)]
        for name, pkg in HELPERS:
            targets.append((name, "./cmd/" + pkg, "-s -w"))
        for name, pkg, ld in targets:
            log = OUT / version / ("build-%s-%s.log" % (arch, name))
            with log.open("w") as fh:
                p = subprocess.run([GO, "build", "-buildvcs=false", "-trimpath",
                                    "-ldflags", ld, "-o", str(stage / name), pkg],
                                   cwd=str(GATEWAY), env=env, stdout=fh,
                                   stderr=subprocess.STDOUT, timeout=600)
            if p.returncode:
                raise SystemExit("构建失败 %s/%s，见 %s" % (arch, name, log))
        for name in SCRIPTS:
            dst = stage / name
            dst.parent.mkdir(parents=True, exist_ok=True)
            dst.write_bytes((GATEWAY / name).read_bytes().replace(b"\r\n", b"\n"))
        archive = assets / ("wb2api-runtime-linux-%s.tar.gz" % arch)
        subprocess.run([sys.executable, str(GATEWAY / "scripts" / "runtime_bundle.py"),
                        "--stage", str(stage), "--version", version,
                        "--arch", arch, "--archive", str(archive)], check=True)
        print("built %s: %s (%d bytes)" % (arch, archive.name, archive.stat().st_size))
    manifest = {"version": version, "commit": commit, "built_at": built_at,
                "assets": {p.name: sha(p) for p in sorted(assets.iterdir()) if p.is_file()}}
    (OUT / version / "build.json").write_text(json.dumps(manifest, indent=2),
                                              encoding="utf-8")
    print(json.dumps(manifest["assets"], indent=2))
    return assets


# ── deploy ──────────────────────────────────────────────────────────────────
def deploy(env, version, expect_commit=None, allow_dirty=False):
    cfg = ENVS[env]
    assets = OUT / version / "assets"
    archive = assets / ("wb2api-runtime-linux-%s.tar.gz" % cfg["arch"])
    if not archive.is_file():
        raise SystemExit("缺产物：%s（先跑 build）" % archive)
    expected_sha = sha(archive)
    built = json.loads((OUT / version / "build.json").read_text(encoding="utf-8"))
    if built["assets"].get(archive.name) != expected_sha:
        raise SystemExit("产物哈希与 build.json 不符，拒绝部署")
    if expect_commit and built["commit"] != expect_commit:
        raise SystemExit("产物提交 %s != 期望 %s" % (built["commit"], expect_commit))

    cli = connect(env)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d-%H%M%S")
    stage = posixpath.join(UPDATES, "runtime-%s-%s" % (version, stamp))
    try:
        _rc, machine, _ = run(cli, "uname -m")
        machine = machine.strip()
        want = {"dev": "aarch64", "prod": "x86_64"}[env]
        if machine != want:
            raise SystemExit("主机架构 %s != 期望 %s" % (machine, want))
        _rc, before, _ = run(cli, "curl -s -m 10 http://127.0.0.1:7863/healthz")
        print("before: %s" % before.strip()[:220])
        _rc, pointer, _ = run(cli, "cat %s/current 2>/dev/null || true" % UPDATES)
        print("current: %s" % pointer.strip())

        run(cli, "mkdir -p %s && chmod 700 %s" % (stage, stage))
        print("uploading %s (%d bytes)" % (archive.name, archive.stat().st_size))
        push(cli, str(archive), stage + ".tar.gz")
        _rc, got, _ = run(cli, "sha256sum %s.tar.gz | cut -d' ' -f1" % stage)
        if got.strip() != expected_sha:
            raise SystemExit("上传后哈希不符：%s != %s" % (got.strip(), expected_sha))
        print("sha256 ok: %s" % expected_sha[:16])
        run(cli, "tar -xzf %s.tar.gz -C %s && rm -f %s.tar.gz" % (stage, stage, stage),
            timeout=300)
        run(cli, "chown -R 10001:10001 %s && chmod 700 %s && chmod 755 %s/wb2api"
            % (stage, stage, stage))

        # manifest 逐文件校验：这是唯一能证明「落盘内容 == 构建产物」的一步。
        verify = (
            "python3 - <<'PYEOF'\n"
            "import hashlib,json,sys\n"
            "m=json.load(open('%s/manifest.json'))\n"
            "bad=[n for n,h in m['files'].items() "
            "if hashlib.sha256(open('%s/'+n,'rb').read()).hexdigest()!=h]\n"
            "print(json.dumps({'version':m.get('version'),'arch':m.get('arch'),"
            "'files':len(m['files']),'bad':bad}))\n"
            "sys.exit(1 if bad else 0)\n"
            "PYEOF\n" % (stage, stage))
        rc, out, err = run(cli, verify, timeout=180)
        print("manifest: %s" % out.strip())
        if rc != 0:
            raise SystemExit("manifest 校验失败：%s %s" % (out, err))

        run(cli, "cp -a %s/current %s/current.bak-%s" % (UPDATES, UPDATES, stamp))
        target = "data/updates/%s/wb2api" % posixpath.basename(stage)
        run(cli, "printf '%%s' %s > %s/current" % (json.dumps(target), UPDATES))
        print("pointer -> %s" % target)

        install = ["if [ -f {s}/{h} ]; then cp -f {s}/{h} {r}/{h} && chmod 755 {r}/{h}; fi".format(
            s=stage, h=name, r=ROOT) for name, _pkg in HELPERS]
        install.append("if [ -d {s}/scripts ]; then cp -rf {s}/scripts/. {r}/scripts/; fi"
                       .format(s=stage, r=ROOT))
        run(cli, " && ".join(install), timeout=180)

        # SIGTERM 优雅停机的等待时长按环境分档。
        #
        # prod 用 900s：在途长流式请求要跑完，docker restart 只等 10s 会切断用户
        # 正在收的回复（v3.10.11 之前的教训）。
        #
        # dev 用 20s：dev 的唯一用途是「快速验证改动」，而 900s 意味着每次部署
        # 只要有一条长会话在跑，容器就要挂 15 分钟——实测用户在这段时间访问会拿到
        # Cloudflare 502（2026-10-18）。dev 上切断在途请求是可以接受的代价。
        stop_grace = 900 if env == "prod" else 20
        run(cli, "docker stop -t %d %s && docker start %s" % (stop_grace, CONTAINER, CONTAINER),
            timeout=stop_grace + 120)
        started = time.time()
        healthy = ""
        while time.time() - started < 240:
            time.sleep(3)
            _rc, healthy, _ = run(cli, "curl -s -m 10 http://127.0.0.1:7863/healthz")
            if version in healthy:
                break
        print("after: %s" % healthy.strip()[:260])
        if version not in healthy:
            raise SystemExit("健康检查未通过；回滚：printf '%s' > %s/current && "
                             "docker stop -t %d %s && docker start %s"
                             % (pointer.strip(), UPDATES, stop_grace, CONTAINER, CONTAINER))
        print("OK: %s 已部署 %s 到 %s" % (version, cfg["label"], target))
    finally:
        cli.close()


# ── verify ──────────────────────────────────────────────────────────────────
# 冒烟模型清单**不硬编码**：从 /v1/models 现取，只测网关自己公布的模型。
# 用户 2026-10-18 明确要求「测试模型只用列表里的」。硬编码清单会随目录变化失准
# （例如 doubao_1_8 已被上游下线却还留在旧清单里）。
def _list_models(cli, key):
    script = r'''
import json, urllib.request
req = urllib.request.Request('http://127.0.0.1:7863/v1/models',
                             headers={'Authorization': 'Bearer __KEY__'})
with urllib.request.urlopen(req, timeout=60) as resp:
    data = json.loads(resp.read())
print(json.dumps([m['id'] for m in data['data']]))
'''.replace("__KEY__", key)
    rc, out, err = run(cli, "docker exec -i %s python3 - <<'PYEOF'\n%s\nPYEOF\n"
                       % (CONTAINER, script), timeout=120)
    try:
        return json.loads(out.strip().splitlines()[-1])
    except Exception:
        print("list models failed: %s %s" % (out[-300:], err[-300:]))
        return []


def _smoke_subset(models):
    """从 /v1/models 里挑每个域 1~2 个**对话**模型做冒烟。

    为什么不全量遍历：verify 是部署后的必经步骤，它会真调上游。全量遍历
    119 个模型（其中大量是 space-bunny / kling-video / minimax-video 这类
    非对话模型）会让每个账号被喂 9~10 次 11134（ErrServer），而
    breaker_threshold=3 → 每个 CN 号各熔断 3 轮、breaker_until 推到 30 分钟后。
    2026-10-18 prod 实测：部署后 realm_servable.cn=false，6 个积分
    4178~5425、直连上游 200 的 CN 号全被自己的冒烟脚本打成「不可服务」。
    冒烟的目的是「验证网关能服务」，不是「验证上游每个模型都活着」——后者
    是非对话模型，本来就该 400/503，把它算成失败既误报又伤账号。
    """
    picks = []
    by_realm = {}
    for m in models:
        realm = "trae" if ":" not in m else m.split(":")[0]
        by_realm.setdefault(realm, []).append(m)
    # 优先挑已知能对话的模型；没有就取该域前两个。
    prefer = {
        "cn": ["cn:glm-5.3", "cn:deepseek-v4.1-flash", "cn:glm-5.3-flash"],
        "global": ["global:deepseek-v4.1-flash", "global:deepseek-v4.1-pro"],
        "trae": ["deepseek-v4.1-flash"],
    }
    for realm, cands in by_realm.items():
        chosen = [m for m in prefer.get(realm, []) if m in cands][:2]
        if not chosen:
            chosen = cands[:1]
        picks.extend(chosen)
    return picks


def verify(env, version, models=None, realm=None, all_models=False):
    """冒烟：healthz + 三域 + 每个域代表模型真调一次。失败返回非零。

    models 省略时从 /v1/models 现取，默认只取每域 1~2 个对话模型（见
    _smoke_subset 的说明：全量遍历会把账号熔断器喂满）；all_models=True
    才逐个遍历（诊断用，会在生产上伤账号，慎用）。realm 可限定 trae/cn/global。
    """
    cli = connect(env)
    failures = []
    try:
        _rc, hz, _ = run(cli, "curl -s -m 10 http://127.0.0.1:7863/healthz")
        print("healthz: %s" % hz.strip()[:300])
        if version not in hz:
            failures.append("healthz 版本不含 %s" % version)
        if '"trae":true' not in hz.replace(" ", ""):
            failures.append("trae 域不可服务")
        _rc, ptr, _ = run(cli, "cat %s/current" % UPDATES)
        print("current: %s" % ptr.strip())

        key, key_id = _mint_key(cli)
        if not key:
            failures.append("建临时密钥失败，无法做真调用")
            models = []
        try:
            todo = models
            if not todo:
                todo = _list_models(cli, key)
                if realm:
                    todo = [m for m in todo
                            if m.split(":")[0] == realm or (realm == "trae" and ":" not in m)]
                print("models from /v1/models: %d" % len(todo))
                if not all_models:
                    todo = _smoke_subset(todo)
                    print("smoke subset: %s" % ", ".join(todo))
            for name in todo:
                rc, out, _ = run(cli, _curl_chat(key, name), timeout=180)
                status = out.strip().splitlines()[-1] if out.strip() else ""
                ok = status.startswith("200")
                print("  %-46s %s" % (name, status or "no response"))
                # 只有「该模型本来能对话」时 200 才是硬要求。全量遍历模式下列表里
                # 混着 space-bunny / kling-video / minimax-video 等非对话模型，它们
                # 返回 4xx/5xx 是正确行为；把这类算失败会让人误以为部署坏了。
                # 单模型失败仍要记录，但不计入退出码（诊断信息照常打印）。
                if not ok:
                    if all_models:
                        print("    (non-chat model or upstream fault; not counted as failure)")
                    else:
                        failures.append("%s -> %s" % (name, status))
        finally:
            if key_id:
                _revoke_key(cli, key_id)
    finally:
        cli.close()
    if failures:
        print("\nFAIL:")
        for f in failures:
            print("  - " + f)
        return 1
    print("\nPASS: %s 冒烟全通过" % ENVS[env]["label"])
    return 0


def _mint_key(cli):
    """在容器内通过管理 socket 建临时 key，返回 (key, id)。

    注意 socket 用**容器内**路径：脚本是 docker exec 进去跑的，宿主路径
    /opt/workbuddy2api/data/api_keys.sock 在容器里映射成 /app/data/api_keys.sock。
    """
    script = r'''
import json, socket, sys
def call(method, path, body=None):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(30)
    s.connect('/app/data/api_keys.sock')
    data = json.dumps(body).encode() if body is not None else b''
    head = ('%s %s HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n'
            'Content-Length: %d\r\nConnection: close\r\n\r\n' % (method, path, len(data)))
    s.sendall(head.encode() + data)
    buf = b''
    while True:
        try:
            c = s.recv(65536)
        except Exception:
            break
        if not c:
            break
        buf += c
    s.close()
    body = buf.split(b'\r\n\r\n', 1)[-1]
    return json.loads(body.decode('utf-8', 'replace'))
r = call('POST', '/keys', {'name': 'devprod-smoke', 'note': 'temporary'})
print(json.dumps(r))
'''
    rc, out, err = run(cli, "docker exec -i %s python3 - <<'PYEOF'\n%s\nPYEOF\n"
                       % (CONTAINER, script), timeout=120)
    try:
        obj = json.loads(out.strip().splitlines()[-1])
    except Exception:
        print("mint key failed: %s %s" % (out[-300:], err[-300:]))
        return None, None
    entry = obj.get("entry") or {}
    return obj.get("key"), entry.get("id")


def _revoke_key(cli, key_id):
    # 用字符串拼接而不是 % 格式化：脚本体内本来就有 '%s %s HTTP/1.1'，
    # 再用 % 注入会把那两处当成占位符。
    script = r'''
import socket
def call(method, path):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(30)
    s.connect('/app/data/api_keys.sock')
    head = (method + ' ' + path + ' HTTP/1.1\r\nHost: localhost\r\n'
            'Content-Length: 0\r\nConnection: close\r\n\r\n')
    s.sendall(head.encode())
    buf = b''
    while True:
        try:
            c = s.recv(65536)
        except Exception:
            break
        if not c:
            break
        buf += c
    s.close()
    return buf.split(b'\r\n', 1)[0].decode()
print(call('DELETE', '/keys/__KEY_ID__'))
'''.replace("__KEY_ID__", str(key_id))
    rc, out, err = run(cli, "docker exec -i %s python3 - <<'PYEOF'\n%s\nPYEOF\n"
                       % (CONTAINER, script), timeout=60)
    print("  key revoked: %s" % (out.strip()[:80] or err.strip()[:80]))


def _curl_chat(key, model):
    # -w 把状态码单独打一行，方便判定；超时给足（kimi-k2 首字节实测 16s）。
    return (
        "curl -s -o /dev/null -w '%%{http_code}' -m 150 "
        "-X POST http://127.0.0.1:7863/v1/chat/completions "
        "-H 'Content-Type: application/json' "
        "-H 'Authorization: Bearer %s' "
        "-d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":8}'"
        % (key, model))


# ── promote ─────────────────────────────────────────────────────────────────
def promote(version, expect_commit=None):
    """把 dev 验证过的同一份构建产物推到 prod（架构不同，各自那份）。"""
    built = OUT / version / "build.json"
    if not built.is_file():
        raise SystemExit("缺 %s —— 先 build 并在 dev 验证" % built)
    print("== 部署到 prod（产物与 dev 同一 commit）==")
    deploy("prod", version, expect_commit=expect_commit)
    return verify("prod", version)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("action", choices=["build", "deploy", "verify", "promote"])
    ap.add_argument("--version", required=True)
    ap.add_argument("--env", choices=["dev", "prod"])
    ap.add_argument("--commit")
    ap.add_argument("--models", nargs="*")
    ap.add_argument("--realm", choices=["trae", "cn", "global"])
    ap.add_argument("--all-models", action="store_true",
                    help="逐个遍历 /v1/models 全部模型（诊断用；会在生产上把账号熔断器喂满）")
    args = ap.parse_args()

    if args.action == "build":
        commit = args.commit or subprocess.check_output(
            ["git", "-C", str(GATEWAY), "rev-parse", "HEAD"]).decode().strip()
        build(args.version, commit)
        return 0
    if args.action == "deploy":
        if not args.env:
            ap.error("deploy 需要 --env dev|prod")
        deploy(args.env, args.version, expect_commit=args.commit)
        return 0
    if args.action == "verify":
        if not args.env:
            ap.error("verify 需要 --env dev|prod")
        return verify(args.env, args.version, args.models, args.realm, args.all_models)
    if args.action == "promote":
        return promote(args.version, expect_commit=args.commit)
    return 0


if __name__ == "__main__":
    sys.exit(main())
