# workbuddy2api

把已授权的 WorkBuddy / CodeBuddy 账号接入常用 AI 工具的自托管中转。网关与管理台合在一个容器里：同一端口同时提供 `/v1/`、`/v1beta/` 接口和 `/admin/` 页面。

源码版本见仓库根 `VERSION`，变更记录见 [CHANGELOG.md](CHANGELOG.md)。站点是否已升级，以 `/healthz` 和管理台显示的版本为准。

本项目基于 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)，保留 MIT 许可证。架构参考 new-api 与 sub2api，按上游实际能力独立实现。

## 能力

**协议与调用**

- OpenAI Chat Completions、Responses、Anthropic Messages、Gemini generateContent 四套协议共用鉴权、账号池、会话隔离、错误处理与用量账本。
- 流式与非流式、函数工具、Responses 自定义工具与命名空间桥接、JSON Schema 校验。
- 每把密钥的频率、并发、排队策略对四协议统一生效。

**账号与模型**

- 多账号池，按 cn / global / trae 三个域分治；每个域独立的端点、鉴权头与风控规则。
- 模型白名单、按模型冷却、熔断、积分到期优先消耗。
- 上游限流分两类处理：带精确恢复时刻的按墙钟对齐，不带时刻的（如 `Model busy`）只记活动时间、不写冷却。

**管理台**

- 账号、密钥、模型、任务、日志、用量六个页面；密钥可重复复制，完整值不进列表与日志。
- 请求明细保留每次重试的已知消费、缺失项与当时的选号依据，可按密钥、模型、状态、请求 ID 筛选。
- 任务页支持手动「跑一次」，并展示每类任务的定时、上次运行、下次运行、是否在跑。

**任务**

- 成长任务支持**动态发现**：以服务端返回的任务列表为准，未在映射表里的新活动按标题/描述推断类型后走同一套「接单 → 上报 → 领奖」链路，新增活动不需要改代码。
- 「成长任务点亮」是一次性任务（无定时、手动触发），一次跑完全部 CN 账号；「成长中心」负责领奖。两者是「先点亮、再领奖」的顺序关系。
- 其余定时任务：签到（WorkBuddy / Trae 分开排程）、猫猫旅行、活跃上报、token 保活、开学季、夜猫子、余额刷新、补偿积分领取。

**运行形态**

- Go 主程序内嵌面板资源，以 `10001:10001` 非 root 运行，不挂载宿主 Docker socket。
- 完整运行包可同时更新网关、内嵌面板、辅助程序与任务脚本。

模型能力由上游决定。支持范围见 [agent 接入](docs/agent-compatibility.md)，实现边界见 [统一架构](docs/architecture-unified.md)。

## 部署

需要 Linux、Docker Engine 与 Compose v2。镜像内含 Python、Bash 和辅助程序，宿主机不需要 Go。

### 方式一：拉预构建镜像（推荐）

```bash
# 1. 准备持久化目录（属主必须是 10001）
sudo install -d -o 10001 -g 10001 -m 700 config auths auths-trash data panel-data
sudo install -o 10001 -g 10001 -m 600 config.example.json config/config.json

# 2. 拉镜像
docker pull ghcr.io/dddmiku/workbuddy2api-maintenance:latest

# 3. 启动（见下方注意事项）
docker compose up -d
```

> **注意：仓库里的 `docker-compose.yml` 用的是 `build:` 而不是 `image:`。**
> 直接 `docker compose up -d` 会**重新构建**镜像，而不是用你刚拉下来的那个。
> 要用预构建镜像，把 compose 里的 `build:` 段替换成：
>
> ```yaml
> services:
>   wb2api:
>     image: ghcr.io/dddmiku/workbuddy2api-maintenance:latest
> ```
>
> 保留其余内容（端口、卷、`security_opt`、`cap_drop`）不变。

镜像由 `.github/workflows/docker-image.yml` 构建，触发条件：每天 UTC 04:00（北京 12:00）、每次打 `v*` tag、手动触发。架构为 amd64 + arm64。

版本号在构建时注入。`/healthz` 与管理台显示的版本就是实际镜像版本；**若显示 `dev`，说明这个镜像是本地 `docker build` 出来的且没传 `--build-arg VERSION`**，不代表发布流程有问题。

### 方式二：从源码构建

```bash
git clone https://github.com/dddmiku/workbuddy2api-maintenance.git workbuddy2api
cd workbuddy2api

sudo install -d -o 10001 -g 10001 -m 700 config auths auths-trash data panel-data
sudo install -o 10001 -g 10001 -m 600 config.example.json config/config.json

export VERSION="$(cat VERSION)"
export COMMIT="$(git rev-parse HEAD)"
export BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
docker compose build
docker compose up -d
```

已有站点升级请按 [迁移与回滚](docs/panel.md#从旧两容器部署迁移) 保留原配置与数据。

### 首次验证

打开 `http://127.0.0.1:7863/admin/`。没有存量管理员凭据时，初始账号与随机密码在 `panel-data/initial-password.txt`，登录后先改密码，再添加上游账号和调用密钥。

```bash
sudo cat panel-data/initial-password.txt
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7863/admin/login   # 期望 200
curl -sS http://127.0.0.1:7863/healthz
```

`/healthz` 返回实际版本、健康账号数和三个域的可服务状态。**尚未添加可用账号时它返回 503，这不等于管理台启动失败。**

也可以在容器里交互添加账号：

```bash
docker compose run --rm --entrypoint /bin/bash wb2api /app/login.sh --realm=cn
```

国际版用 `--realm=global`，Trae 用 `--realm=trae`。

默认只发布回环端口，远程访问请自备 [HTTPS 反向代理](docs/panel.md#反向代理)。

## 持久化目录

| 宿主目录 | 容器目录 | 内容 |
|---|---|---|
| `config/` | `/app/config` | 实际配置 `config.json` |
| `auths/` | `/app/auths` | 上游授权账号 |
| `auths-trash/` | `/app/auths-trash` | 回收的账号文件 |
| `data/` | `/app/data` | 密钥库及加密文件、状态、用量、日志、更新版本 |
| `panel-data/` | `/app/panel-data` | 管理员凭据、会话状态、登录流程数据 |

五个目录都需要 `10001:10001` 读写权限。配置挂载整个 `./config:/app/config` 而非单文件——原子替换后单文件挂载仍会读到旧内容。配置里的相对路径以 `/app` 为工作目录，模板中的 `./data`、`./auths` 不需要改。

## 客户端接入

| 配置项 | 值 |
|---|---|
| OpenAI Base URL | `http://127.0.0.1:7863/v1`，远程时换成自己的 HTTPS 地址 |
| 调用密钥 | 管理台创建的 API key |
| 模型 | `/v1/models` 返回的完整 id，保留 `cn:` / `global:` / `trae:` 前缀 |
| Codex 接口类型 | `responses` |
| Anthropic 兼容路径 | `/v1/messages`，接受 `x-api-key` 或 Bearer |
| Gemini Base URL | `http://127.0.0.1:7863/v1beta`，用 `x-goog-api-key` |

```bash
curl -sS http://127.0.0.1:7863/v1/models \
  -H "Authorization: Bearer $WORKBUDDY_API_KEY"
curl -sS http://127.0.0.1:7863/v1/capabilities \
  -H "Authorization: Bearer $WORKBUDDY_API_KEY"
```

模型详情与列表共用同一白名单。Gemini 用 `/v1beta/models`。NarraFork 六模式的完整地址与鉴权见 [配置指南](docs/narrafork.md)。日志里的 `rid=` 可与响应头 `X-Request-ID` 对应。工具终态、计量与压缩语义见 [agent 接入](docs/agent-compatibility.md)。

## 密钥、用量与升级

密钥库默认启用，空库拒绝调用。完整密钥值不进列表也不进日志。备份 `data/` 时**要一并保留密钥库与 `.enc-key`**，否则恢复后所有密钥失效。

用量一律采信上游上报值：缓存属于输入、思考属于输出，不重复相加。失败与取消保留已知消费，缺失部分标为「未完整上报」；客户端隐藏流式用量不会关闭内部记账。

在密钥页「设置限流」可限制请求数、并发与等待时间，已有密钥默认不限。「请求明细」保留 7 天、最多 10000 条或 64 MiB，与累计账本独立；未上报的消费显示未知，不乘固定倍率补算。详见 [限流与明细](docs/operations-observability.md)。

### 热更新

**默认开启**（`config update.enabled` 缺省 `true`）。管理台「版本与热更新」页可直接检查并应用新版本：

- 新进程先接管监听套接字，旧进程把在途请求（含长 SSE 对话）跑完才退出——**升级不打断正在进行的对话**，容器也不重建。
- 依赖 Release 里的**完整运行包** `wb2api-runtime-linux-<arch>.tar.gz`，不是裸二进制。发布时漏 `--runtime` 会表现为面板报「没有本机架构的二进制资产」且 `asset_name` 为空；发布工具已把这条做成硬门禁。
- 下载走 `update.proxy` 指定的加速前缀（默认 `https://gh-proxy.com`，设成 `-` 表示直连）。国内直连 GitHub Releases 经常超时，代理不可用时热更新会一直卡在下载。

关闭热更新（`update.enabled=false`）后 `/update/*` 一律拒绝，只能手工部署：

1. 本地构建运行包：`python tools/private_gateway_release_20260925.py prepare --tag <tag> --runtime`，产物为 `wb2api-runtime-linux-<arch>.tar.gz`，构建时注入发布公钥；
2. 上传到服务器任意临时目录，解压到 `/opt/workbuddy2api/data/updates/runtime-<时间戳>/`。**属主与权限必须对齐容器用户**：`chown -R 10001:10001 <目录> && chmod 700 <目录>`——目录归 root 且 0700 时容器内的 app 用户进不去，表现为「重启后仍是旧版本」；
3. 校验 `manifest.json` 里每个文件的 sha256 与解压结果一致（`wb2api` 需 `0755`）；
4. 把 `data/updates/current` 指向新目录里的 `wb2api`（改前记下旧值，便于回滚）；
5. `docker restart workbuddy2api`，核对 `/healthz` 的版本与提交；未就绪就把 `current` 改回旧值再重启。

旧运行目录不会自动清理，可定期删除 `data/updates/runtime-*` 里不再需要回滚的那些。

**镜像与运行包是两条独立的更新路径**：热更新只替换运行包（网关二进制、内嵌面板、辅助程序、脚本），不换基础镜像。基础镜像、Python/Bash 依赖或 `docker-entrypoint.sh` 有改动时，必须重新 `docker pull` 并重启容器。

## 开发与构建

```bash
go test ./...
go vet ./...
go test -race ./...              # 需要 cgo 与 C 编译器
python3 panel/build.py           # 生成并提交 panel/index.html
python3 -m unittest discover -s panel -p 'test_*.py' -v
python3 -m unittest discover -s scripts -p 'test_*.py' -v
```

编译主程序前先跑 `panel/build.py`，否则内嵌资源是旧的。

`tests/client-contracts/` 用固定版本官方 SDK 与回环假上游做持续兼容测试，只使用合成内容，不读生产凭据。CI 运行全包测试、vet 与面板回归；构建工作流保存 Actions artifact，正式 Release 由发布流程另行创建并上传运行包。AI 治理工作流默认关闭，不随 issue/PR 自动运行。

合成回归、真实客户端测试、生产迁移三者分别验收：源码支持某个接口，不代表任意模型、百万上下文恢复或生产切换已经验证通过。

## 许可证

[MIT](LICENSE)。保留原项目版权；依赖遵循各自许可证，参考项目没有作为整套源码并入。
