# 官方客户端契约测试

eni: 这套测试让固定版本的官方 SDK 调用真实网关 Handler，网关再调用禁止拨号的内存假上游。日常运行无需生产账号、真实 API key、外部模型额度、narrafork 安装包或 Codex 登录。

## 运行

在仓库根目录使用 Node.js 24.15.0，以及 `go.mod` 指定的 Go 工具链：

```sh
npm --prefix tests/client-contracts ci --ignore-scripts --no-audit --no-fund
node scripts/client_contracts.mjs
node scripts/client_contracts.mjs --prove-oracle
```

`--prove-oracle` 故意把客户端收到的缓存命中数从 40 改成 0，而真实网关账本仍为 40。它要求指定断言失败，才返回成功；构建错误、启动错误或没有运行测试不能冒充“已检出回归”。正常矩阵和这个反例分别记录结果。

在安装了 C 编译器的环境中，还可运行夹具的 Go race detector：

```sh
node scripts/client_contracts.mjs --race
```

只运行一个协议时：

```sh
node scripts/client_contracts.mjs --test-name-pattern="^anthropic messages"
```

若 Go 不在 PATH，可将 `CLIENT_CONTRACT_GO` 设置为已安装 Go 可执行文件的绝对路径。脚本在独立临时目录构建并启动夹具，结束时关闭进程、等待输出收尾，再校验路径并清理该临时目录。它不启动网关主程序、不加载应用配置，也不读取已有账号目录。

需要归档到项目外的证据目录时，可设置 `CLIENT_CONTRACT_OUTPUT_ROOT`，值必须为绝对路径。它只改变报告位置，运行用的临时目录仍单独建立与清理；CI 默认使用本目录的 `.output/`。

## 覆盖的客户端和行为

| 官方包 | 固定版本 | 协议 |
|---|---|---|
| `openai` | `7.23.0` | Chat Completions、Responses |
| `@anthropic-ai/sdk` | `0.128.0` | Messages |
| `@google/genai` | `2.24.0` | Gemini generateContent |

直接依赖使用精确版本，传递依赖及 tarball integrity 固定在 `package-lock.json`。安装时强制忽略 lifecycle scripts，依赖升级必须经过同一套验证。

每种协议有六个场景，共 24 项：

| 场景 | 核心断言 |
|---|---|
| 流式文本、思考与用量 | 真实 SDK 读到正文和思考；终态唯一；输入、缓存和输出计数保留 |
| 双工具往返 | 官方流式 helper/解析器收到两个完整工具；两个回执按 ID 配对传至上游；第二轮成功 |
| 参数完成后再损坏 | 先给出两个能解析的 JSON，再追加无效后缀；验证整个工具组都没有提前交付，错误后已知用量仍入账 |
| 主动取消 | SDK 的 AbortSignal 立即取消实际转发上下文；不等 SDK 超时；不冒充成功，保留已报告用量 |
| 正常 gzip 请求 | SDK 先构造 JSON，再由受控 fetch 层 gzip；实际 Handler 解压后，完整输入到达假上游 |
| 损坏 gzip 请求 | 官方客户端收到 HTTP 400；上游调用数和计费请求数均为零 |

工具失败用例使用流屏障：完整工具 JSON 后还有可见文本标记。客户端收到标记时必须仍然没有工具事件；测试随后释放损坏后缀。这个判断不依赖固定 sleep。

用量夹具明确给定输入 100、其中缓存 40、输出 25、其中推理 5。账本总量是 125，缓存和推理不重复相加。Anthropic 对应非缓存输入 60 + 缓存 40；Gemini 对应可见输出 20 + 推理 5。取消和流错误也保留已经观测到的计数。账本没有单独的推理累计列。

`openai` 的 Chat 原始流解析器可保留 `reasoning_content` 这个网关扩展；它不是原生 OpenAI Chat 推理字段。Responses 的推理使用原生事件。Google SDK 2.24.0 会丢弃 SSE `error` 和 `finishMessage` 细节，所以测试要求原始响应保留错误、SDK 读到 `finishReason=OTHER`，而不是要求 SDK 自动抛出详细异常。

## 结果与 CI

结果写入本目录被 Git 忽略的 `.output/`：

- `normal/results.json`：官方包版本、逐场景结果、实际上游请求、客户端响应及账本。
- `normal/runner.json`：客户端和夹具退出状态、临时目录清理结果。
- `normal-race/`：启用 race detector 时的同类记录；Go 夹具退出异常会使整次任务失败。
- `drop-cache/`：受控反例的失败断言以及 `expected_regression_detected`。

[client-compatibility.yml](../../.github/workflows/client-compatibility.yml) 仅允许已核验的 `dddmiku/workbuddy2api-maintenance`、repository ID `1386246860` 且当前仍为 private 的仓库运行。相关 push/PR 及手动触发均受同一限制，检出前和上传测试证据前还会读取当前私有状态。

CI 在 Linux 和 Windows 各跑一次；Linux 另跑常规全包测试、vet 与面板回归。race detector 由维护流程在服务器的隔离源码副本执行，既可覆盖完整 Go 测试，也可用上面的 `--race` 运行官方 SDK 夹具。只有 `contents: read` 权限，checkout 不持久化凭据，测试子进程移除供应商凭据环境变量。工作流不发布 Release、不调用模型、不自动升级依赖、不创建 issue 或发表评论。手动选择 `dependency_report` 只运行 `npm outdated` 并保存查询结果。

## 与外部真实客户端回放的区别

这 24 项确实使用官方 SDK，且确实经过当前仓库的 Handler；上游回复是自有合成夹具。测试不验证真实模型质量、官方上游可用性、精确 tokenizer、完整 GUI 或生产部署状态。

narrafork 原包、完整 Codex CLI 和长会话自动压缩仍属于自愿的外部验收。之前本机 artifacts 中的 NF 原适配器回放是独立证据，既不是这里的官方 SDK，也不是 CI 的依赖。仓库不包含 NF 提取源码或第三方大段实现。这里的“压缩”明确指 HTTP gzip 请求，不代表测试了原生 `/responses/compact` 或真实长会话自动压缩。

新增场景时，在 `fixture/main.go` 提供确定的上游行为，在 `contracts.test.mjs` 使用官方 SDK 的公开方法验证可见结果。不要伪造 SDK 返回值或绕开 Handler，也不要把真实会话载荷、账号或密钥加入夹具。

官方 OpenAI 接口参考：[流式响应](https://developers.openai.com/api/docs/guides/streaming-responses)、[工具调用与回执](https://developers.openai.com/api/docs/guides/function-calling)。测试细节以锁定版本 SDK 的实际行为和仓库契约为准。
