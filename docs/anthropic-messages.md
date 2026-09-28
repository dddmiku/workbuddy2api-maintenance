# Anthropic messages 兼容说明

`POST /v1/messages` 复用网关的账号调度、权限、原始计量和上游错误处理。官方 JavaScript SDK 的 `baseURL` 应指向站点根地址，由 SDK 添加 `/v1/messages`。`x-api-key` 可用于鉴权；显式 `Authorization` 始终优先，包括其值无效时。

nf 的 Anthropic 兼容与 ClaudeCode 中转共用该入口，但 nf 自行追加的是 `/messages`，因此两者在 nf 中都填到 `/v1`。前者通常用 `x-api-key`，后者用 Bearer 并带 `?beta=true`。`beta=true` 不代表所有官方平台 beta 能力都可用，详见 [六模式配置](narrafork.md)。

## 消息角色

`messages[].role` 接受 `user`、`assistant`，以及两种客户端常见写法（大小写与首尾空白不敏感）：

- `system` / `developer`：**并回系统提示**——与顶层 `system` 合并成一条，和它在数组里的位置无关，因此不会出现「第二条 system 被上游忽略」。
- `tool` / `function`：按 `user` 轮处理（工具结果在 Anthropic 语义里属于 user 轮），工具配对规则不变。

这条兼容是实测需要：Claude Code 2.1.283 **遇到它不认识的模型名**（中转站的常态，例如 `global:deepseek-v4.1-flash[1M]`）会把系统提示的 Environment 段单独作为一条 `role:"system"` 的消息放进 `messages`；认识的模型名下同一段走顶层 `system` 字段。官方端点容忍这种写法，早期网关按「只能 user/assistant」整条 400，客户端显示 `400 messages[1].role must be user or assistant`，且每一轮都复现。

只有 `system` 而没有 user/assistant 轮的请求仍拒绝（上游无法成立），未知角色（如 `observer`）同样拒绝；错误文案为 `messages[N].role must be user, assistant, system or tool`。

## nf 思考与上下文设置

支持 nf 的精确保留请求 `context_management={"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`：它表示保留全部思考，不执行裁剪。其它主动清理/压缩/服务端上下文动作仍拒绝。`output_config.effort` 接受 low、medium、high、xhigh、max，后续按实际模型的既有规则处理，不伪造精确思考预算。

## 流式内容与工具

正文和明文思考正常流式输出。工具调用先缓冲，直到上游整轮结束、全部工具身份及参数 JSON 都校验通过，再按工具顺序输出各自完整的 `content_block_start`、`content_block_delta`、`content_block_stop`，最后输出消息终态。

这一顺序同时满足执行安全和官方 SDK 的完成回调约定。官方 `@anthropic-ai/sdk` 0.128.0 的 `contentBlock` 回调按最近内容块触发；同时打开多个工具、最后集中关闭时，回调可能重复指向最后一个工具，即使 `finalMessage().content` 看上去正确。

缓冲期间如持续收到工具片段，距上次实际下行事件超过十秒时发送标准 `ping` 保活。工具生成进度暂不交付为可执行内容。任意工具残缺或上游错误都会产生失败，不能靠提前发送工具结束事件获得表面兼容。

上游没有原生思考签名时，明文 thinking 使用空签名，不生成伪造签名。只有密文或不可恢复签名的历史仍明确拒绝。

## 用量

`message_start` 的计数是初始快照，尚无上游计量或缓存拆分尚未明确时不抢报普通输入；`message_delta.usage` 携带实际累计用量。客户端应按字段覆盖累计值，不把各帧相加。nf 0.7.7 会忽略部分合法零修正，因此不能把未知缓存拆分的总输入先当作普通输入发送；最终真实值不为补偿客户端而改写。

Anthropic 的 `input_tokens` 表示普通输入，缓存读取和缓存创建分别报告。例如上游 OpenAI 口径总输入为 100、其中缓存读取 60 时，messages 输出 `input_tokens: 40` 与 `cache_read_input_tokens: 60`。思考已包含在输出总量时不再重复相加。

官方 SDK 0.128.0 已通过以下两种流式形状的验证：

- 首帧输入/输出为零，末帧提供输入、缓存和输出累计计数。
- 首帧已有输入及缓存计数，末帧只提供输出计数。

实际用量仍缺失时，原始末帧 SSE 的 usage 带 `gateway_usage_incomplete: true`。官方 SDK 的 `finalMessage()` 只合并协议规定的用量字段，不合并这类扩展字段；需要该完整性信息的集成应观察原始 `message_delta` 事件。首帧不写临时完整性标记，以免 SDK 将临时状态保留到最终消息。

服务端账本以实测用量与完整性记录为准。网关不估算、不放大计数，也不把精确计数接口伪装成可用；`/v1/messages/count_tokens` 仍明确返回不支持。

## 验证范围

2026-09-25 使用固定官方 `@anthropic-ai/sdk` 0.128.0，通过本地实际 Handler 和回环假上游验证：正文、思考空签名、晚到用量、缓存拆分、两个工具各完成一次、残缺工具失败、缺失用量原始标记与非流式消息。

对应 Go 测试为 `TestMessagesOfficialSDKContract`；设置 `WB2API_ANTHROPIC_SDK_CONTRACT` 指向已安装固定 SDK 的 `handler-contract.mjs` 后执行。未配置该外部验证依赖时，此项明确跳过；普通 Go 回归仍检查顺序内容块、全量参数校验与心跳。

该验证覆盖标准 messages SDK 接口，不表示原生 Anthropic 服务端工具、存储、MCP、容器或所有 beta 功能均受支持。

本轮还使用 nf 原始请求构建与解析函数做回环假上游验证。nf 的模型测试不发送完整 Agent 参数，短测试通过不能替代正式工具轮；nf 对部分 EOF/错误字段的处理也有自身限制。没有把这些回放写成完整 GUI 或本轮真实模型验证。
