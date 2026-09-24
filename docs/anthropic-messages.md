# Anthropic messages 兼容说明

`POST /v1/messages` 复用网关的账号调度、权限、原始计量和上游错误处理。官方 JavaScript SDK 的 `baseURL` 应指向站点根地址，由 SDK 添加 `/v1/messages`。`x-api-key` 可用于鉴权；显式 `Authorization` 始终优先，包括其值无效时。

## 流式内容与工具

正文和明文思考正常流式输出。工具调用先缓冲，直到上游整轮结束、全部工具身份及参数 JSON 都校验通过，再按工具顺序输出各自完整的 `content_block_start`、`content_block_delta`、`content_block_stop`，最后输出消息终态。

这一顺序同时满足执行安全和官方 SDK 的完成回调约定。官方 `@anthropic-ai/sdk` 0.128.0 的 `contentBlock` 回调按最近内容块触发；同时打开多个工具、最后集中关闭时，回调可能重复指向最后一个工具，即使 `finalMessage().content` 看上去正确。

缓冲期间如持续收到工具片段，距上次实际下行事件超过十秒时发送标准 `ping` 保活。工具生成进度暂不交付为可执行内容。任意工具残缺或上游错误都会产生失败，不能靠提前发送工具结束事件获得表面兼容。

上游没有原生思考签名时，明文 thinking 使用空签名，不生成伪造签名。只有密文或不可恢复签名的历史仍明确拒绝。

## 用量

`message_start` 的计数是初始快照，尚无上游计量时为零；`message_delta.usage` 携带实际累计用量。客户端应按字段覆盖累计值，不把各帧相加。

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
