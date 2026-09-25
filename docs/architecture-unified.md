# 统一运行时与协议适配

本文描述已实现的源码与构建契约，生产迁移和真实客户端验收单独记录。新部署主线为一个容器、一个 `7863` 端口和五个持久化目录。

## 模块边界

| 模块 | 职责 |
|---|---|
| `cmd/server`、`internal/server` | `/v1/`、`/v1beta/` 协议入口、鉴权、转换、完整性校验、流式写出 |
| `internal/pool`、`internal/session`、`internal/upstream` | 统一调度、会话隔离、上游连接、错误与限流策略 |
| `internal/usage`、`internal/runlog` | 原始消费账本、失败/缺失状态、有界滚动日志 |
| `panel/assets.go`、`internal/panelruntime` | 内嵌资源、私有 Python 子进程、`/admin/` 代理及生命周期 |
| `panel/native_runtime.py` | 仅允许已列出的登录、积分、日志、同版本重载操作 |
| `internal/hotupdate` | 私有下载、整包校验、候选就绪、监听器交接与旧进程收尾 |

四个协议都进入公共 chat 执行模块，不各自复制换号或记账循环。原始用量在响应翻译前观察；原生 Chat 的展示过滤发生在工具契约校验之后。协议边界详见 [agent 接入](agent-compatibility.md)。

工具身份和参数在整组校验后才交付，正文/思考保持流式。`finishResponseWriters` 先收尾外层校验，再收尾 messages/Gemini 等输出适配器，最后检查网络 writer；这一步发生在公共记账前。适配器收尾幂等，不重复发成功终态，不因最后一帧写失败丢掉实际消费。

管理台资源只从 `panel/assets.go` 的固定清单嵌入，运行时释放到私有临时目录，通过权限为 `0600` 的 Unix socket 通信。管理凭据继续使用原格式。入口校验可信代理后重建转发头，子进程没有宿主 Docker 控制权限。

配置使用 `./config:/app/config`，实际文件为 `/app/config/config.json`；`auths`、`auths-trash`、`data`、`panel-data` 分别挂载。运行身份为 `10001:10001`。运行包和临时面板资源不包含这些持久化目录。

v2.3.0 沿用已完成的单容器方案。`docker-compose.published.yml` 是历史双容器部署文件，不是当前启动入口；现有统一运行版无需再次做迁移。

## 完整运行包

唯一文件清单位于 `scripts/runtime_bundle.py` 的 `FILES`，与 Go 解包器固定允许表对应：

| 内容 | 文件 |
|---|---|
| 主程序与 5 个辅助程序 | `wb2api`、`login`、`credit`、`signin_bin`、`trial_bin`、`activity_bin` |
| 4 个 shell 脚本 | `login.sh`、`credit.sh`、`signin.sh`、`trial.sh` |
| 5 个 Python 脚本 | `scripts/global_region.py`、`task_common.py`、`task_runner.py`、`school_open_day_2026.py`、`growth_center.py` |
| 清单 | `manifest.json`：格式、版本、Linux 架构与每个文件的 SHA-256 |

面板已嵌入 `wb2api`，不再产生独立面板版本包。`runtime_bundle.py` 是构建工具，本身不在上述运行清单中；基础镜像、解释器和 PID 1 的 `docker-entrypoint.sh` 由完整镜像升级。

发布资产名为 `wb2api-runtime-linux-amd64.tar.gz`、`wb2api-runtime-linux-arm64.tar.gz`。下载器使用私有 GitHub asset API 和 Release 摘要，凭据只发给指定仓库 API 路径，跳转时不泄露给 CDN。解包拒绝未知/重复文件、路径越界、链接、错误格式/版本/架构、缺失文件及摘要不符，并限制压缩与解包体积。

验证后的候选进程启动同版面板并继承公共及管理监听器；完整就绪后才提交 `current`。失败回收候选、保留旧版本。旧请求、任务和状态提交按统一 15 分钟预算收尾，Compose 留出 16 分钟停止宽限。配置重载复用这一流程；首次两容器迁移仍使用完整镜像与维护窗口。

## 私有构建与校验

根 `VERSION` 记录本轮发行目标 `v2.3.0`，面板元数据为 `2.3.0`。Go 的实际版本、提交和时间由构建参数注入；未注入时仍为 `dev`。本地正式构建从 `VERSION` 读取，tag 构建使用对应 tag；手动非 tag 的 CI 构建保持开发标识。目标版本文件不替代实际二进制和运行清单核验。

`.github/workflows/build.yml` 仅允许 `dddmiku/workbuddy2api-maintenance`，同时检查事件中的私有标识和 GitHub API 的当前可见性。入口只有手动运行和版本 tag。每次上传产物前再次检查仓库仍为私有；权限仅为仓库内容读取，不拥有包或 Release 发布权限。

| 阶段 | 检查或产物 |
|---|---|
| 工具链 | Go 1.26.8、Python 3.12，Node.js 用于前端行为回归 |
| 源码校验 | 全量 Go 测试、vet、race；面板与脚本 unittest |
| 资源一致性 | 重新生成 `panel/index.html`，要求与提交内容一致 |
| 打包 | 两架构编译固定 6 个程序，复制固定 4 个 shell/5 个 Python 文件，生成 manifest 与压缩包 |
| 交付 | 私有 Actions artifact 内的两个运行包和 `SHA256SUMS.txt`，保留 14 天 |

该流程没有每日触发、GHCR 登录、镜像推送或自动 Release 写入。正式私有 Release 由维护发布流程完成。历史 `2.1.29` 已补发私有 Release，不通过旧 tag 的历史构建流程重新发布镜像。

ai 治理是另一个默认关闭的手动流程。只有同时允许外发和条目修改、并选择目标编号，才会把指定 issue/PR 及仓库相关上下文发送到配置的 ai 服务。旧 action 的 dry-run 只覆盖归并层，其他分支仍可能分类、评论、修改或关闭条目，因此不能当作全流程只读预览。它不再响应 issue/PR 自动事件，构建也不调用它。GitHub 端启用状态与源码设置分别核验。

生产验收另行核对实际版本/提交/文件摘要、唯一容器、账号和密钥保留、累计用量、管理登录、任务及指定模型的真实客户端链路。合成协议测试或低阈值摘要恢复不能替代百万上下文恢复证据。

## 参考范围

| 主要参考 | 本项目借鉴的设计 |
|---|---|
| new-api `d04c118c8803f49e0c9bab74dcf5b5efeab9464a` | 流式生命周期、Gemini 格式与历史配对、协议适配边界、原始用量与展示分离 |
| sub2api `a3eb7ef302961cba716dc78b39b93b60c467db0e` | 统一鉴权与模型可见性、SSE 心跳兼容、能力声明、工具块往返、错误恢复与上下文超限语义 |
| workbuddy2api-panel `dbd7c6800ed8071d7dd617d456b6041294781fee` | 辅助对照同源面板功能和单服务部署取舍 |

参考快照时间为 2026-09-25。分别保留其 AGPL-3.0、LGPL-3.0、MIT 许可边界；本轮按本项目模块独立实现，没有直接并入前两个项目的源码。尤其没有照搬默认模型窗口、原生 compact 或完整 Anthropic 平台能力。
