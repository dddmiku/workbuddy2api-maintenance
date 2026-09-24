# workbuddy2api

将已授权的 WorkBuddy / CodeBuddy 账号接入常用 ai 工具的自托管中转。网关与管理台合在一个镜像、一个容器内：同一端口提供 `/v1/` 接口和 `/admin/` 页面。

维护仓库为私有的 [dddmiku/workbuddy2api-maintenance](https://github.com/dddmiku/workbuddy2api-maintenance)。本项目基于 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)，保留 MIT 许可证；架构改进重点参考 new-api 与 sub2api，按实际上游能力独立实现。

## 能力

- OpenAI chat、Responses 与 Anthropic messages 共用鉴权、账号池、会话隔离、错误处理和原始用量账本。
- 支持流式与非流式响应、函数工具、Responses 自定义工具和命名空间桥接、JSON schema 校验。
- 管理台提供账号、密钥复制、模型白名单、到期时间、重复推理保护、任务、日志和用量管理。
- Go 主程序内嵌面板资源并托管 Python 子进程；以 `10001:10001` 运行，不挂载宿主 Docker socket。
- 完整运行包同时更新网关、内嵌管理台、辅助程序和任务脚本；候选就绪后再提交当前版本指针。

模型能力由上游决定。支持范围见 [agent 接入](docs/agent-compatibility.md)，实现边界见 [统一架构](docs/architecture-unified.md)。

## 新部署

需要 Linux、Docker Engine、Compose v2 和私有仓库读取权限。镜像包含 Python、Bash 和辅助程序，宿主机不用安装 Go。源码与 CI 使用 Go 1.26.8、Python 3.12，前端行为测试另用 Node.js。

以下步骤用于新目录；已有站点按 [迁移与回滚](docs/panel.md#从旧两容器部署迁移) 保留原配置和数据。

```bash
git clone https://github.com/dddmiku/workbuddy2api-maintenance.git workbuddy2api
cd workbuddy2api
sudo install -d -o 10001 -g 10001 -m 700 config auths auths-trash data panel-data
sudo install -o 10001 -g 10001 -m 600 config.example.json config/config.json
docker compose build
docker compose up -d
```

打开 `http://127.0.0.1:7863/admin/`。没有存量管理员凭据时，初始账号和随机密码保存在 `panel-data/initial-password.txt`；登录后修改密码，再添加上游账号和调用密钥。

```bash
sudo cat panel-data/initial-password.txt
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7863/admin/login
curl -sS http://127.0.0.1:7863/healthz
```

登录页应返回 200。`/healthz` 返回实际版本与账号可服务状态；尚未添加可用账号时，503 不等于管理台启动失败。默认只发布回环端口，远程访问使用自己的 [HTTPS 反向代理](docs/panel.md#反向代理)。

也可以在镜像中交互添加账号，再从管理台重载运行版本：

```bash
docker compose run --rm --entrypoint /bin/bash wb2api /app/login.sh --realm=cn
```

国际版选择 `--realm=global`。

## 持久化目录

| 宿主目录 | 容器目录 | 内容 |
|---|---|---|
| `config/` | `/app/config` | 实际配置 `config.json` |
| `auths/` | `/app/auths` | 上游授权账号 |
| `auths-trash/` | `/app/auths-trash` | 回收的账号文件 |
| `data/` | `/app/data` | 密钥库及加密文件、状态、用量、日志、更新版本 |
| `panel-data/` | `/app/panel-data` | 管理员凭据、会话状态、登录流程数据 |

五个目录需要 `10001:10001` 读写权限。配置挂载整个 `./config:/app/config`，避免原子替换后仍读取旧文件。相对配置路径仍以 `/app` 为工作目录，模板中的 `./data`、`./auths` 不需要改成 `../data`。

## 客户端接入

| 配置项 | 值 |
|---|---|
| OpenAI Base URL | `http://127.0.0.1:7863/v1`，远程时替换为自己的 HTTPS 地址 |
| 调用密钥 | 管理台创建的 API key，或首次建库迁移的已有配置密钥 |
| 模型 | `/v1/models` 中的完整 id，保留 `cn:` 或 `global:` 前缀 |
| Codex 接口类型 | `responses` |
| Anthropic 兼容路径 | `/v1/messages`，接受 `x-api-key` 或显式 Bearer |

以下示例读取已设置的 `WORKBUDDY_API_KEY`：

```bash
curl -sS http://127.0.0.1:7863/v1/models \
  -H "Authorization: Bearer $WORKBUDDY_API_KEY"
curl -sS http://127.0.0.1:7863/v1/capabilities \
  -H "Authorization: Bearer $WORKBUDDY_API_KEY"
```

模型详情 `/v1/models/{model}` 与列表使用相同白名单。`X-Request-ID` 可与日志 `rid=` 关联。工具终态、计量与压缩语义见 [agent 接入](docs/agent-compatibility.md)。

## 密钥、用量与升级

密钥库默认启用，空库拒绝调用。管理员可以再次复制可恢复的密钥；完整值不放进列表或日志。备份 `data/` 时同时保留密钥库及 `.enc-key` 文件。用量采用上游实际报告值；缓存属于输入、思考属于输出，不重复相加。失败和取消保留已知消费，缺失部分标成未完整上报；客户端隐藏流式用量不会关闭内部记账。

统一运行版从私有 Release 下载 `wb2api-runtime-linux-amd64.tar.gz` 或 `wb2api-runtime-linux-arm64.tar.gz`，校验压缩包与固定文件清单后交接。`update.repo` 默认指向私有维护仓库，读取凭据只放在本机实际配置。旧 `2.1.29` 已保留为私有 Release，供历史维护与回滚。

运行包包含内嵌面板和辅助程序；基础镜像、Python/Bash 或 `docker-entrypoint.sh` 改动仍需重建镜像。首次从旧两容器迁移同样使用完整镜像。候选就绪后旧进程最多等待 15 分钟收尾，超时仍可能中断。详见 [管理台部署](docs/panel.md) 和 [配置说明](docs/configuration.md#热更新)。

## 开发与构建

```bash
go test ./...
go vet ./...
go test -race ./...
python3 panel/build.py
python3 -m unittest discover -s panel -p 'test_*.py' -v
python3 -m unittest discover -s scripts -p 'test_*.py' -v
```

`-race` 需要 cgo 与 C 编译器。编译主程序前先生成并提交 `panel/index.html`。构建流程仅在指定私有仓库手动或版本 tag 触发，生成两个架构的运行包作为私有 Actions artifacts，不自动发布 Release 或公共镜像。ai 治理默认关闭，不随 issue/PR 自动运行。校验设计见 [统一架构](docs/architecture-unified.md)。

合成回归、真实客户端测试和生产迁移分别验收；源码支持某接口，不代表任意模型、百万上下文恢复或生产切换已经验证通过。

## 许可证

[MIT](LICENSE)。保留原项目版权；依赖遵循各自许可证，参考项目没有作为整套源码并入。
