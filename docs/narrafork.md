# NarraFork 六模式配置

适用于 v2.3.0 网关与本轮核对的 nf 0.7.7。六种界面模式对应四种协议，不是六套独立模型或账号池。下面以本机 `http://127.0.0.1:7863` 为例；远程使用时替换为自己的 HTTPS 站点地址。

## 填好地址、密钥与模型

先在管理台创建调用密钥，查询可见模型，保留完整的 `cn:` 或 `global:` 前缀。不要填写 WorkBuddy 登录令牌，也不要把 `global:hy3` 改成一个虚构的 Gemini/Claude 模型名。

| nf 模式 | 在 nf 中填写的 Base URL | 实际生成接口 | nf 使用的认证头 |
|---|---|---|---|
| Anthropic 兼容 | `http://127.0.0.1:7863/v1` | `/v1/messages` | `x-api-key` |
| Codex 中转 | `http://127.0.0.1:7863/v1` | `/v1/responses` | `Authorization: Bearer` |
| Responses 兼容 | `http://127.0.0.1:7863/v1` | `/v1/responses` | `Authorization: Bearer` |
| ClaudeCode 中转 | `http://127.0.0.1:7863/v1` | `/v1/messages?beta=true` | `Authorization: Bearer` |
| Completions 老旧兼容 | `http://127.0.0.1:7863/v1` | `/v1/chat/completions` | `Authorization: Bearer` |
| Gemini 兼容 | `http://127.0.0.1:7863/v1beta` | `/v1beta/models/{模型}:streamGenerateContent?alt=sse` | `x-goog-api-key` |

这里写的是 **nf 的 Base URL**。nf 的 Anthropic 适配器自行追加 `/messages`，所以需要填到 `/v1`；官方 Anthropic SDK 自行追加 `/v1/messages`，才使用站点根地址。两者不要混用。Completions 模式实际调用 Chat Completions，不是 `/v1/completions`。

模型在 nf 中可能另有供应商前缀；它不替代网关的地区前缀。模型列表填写网关返回的完整 id；手工引用 nf 供应商时，保持类似 `供应商前缀:global:hy3` 的层级。

## 各模式需要注意什么

- **Codex 中转**：使用 HTTP Responses。网关没有原生 Responses WebSocket 服务，不需要打开 nf 的 Codex WebSocket 选项。默认 `store=false` 可用；不支持服务端存储和 `previous_response_id` 续接。
- **Codex 默认工具**：nf 可能自动声明 `web_search`、`image_generation`。网关接受这些默认声明后过滤，并通过 `X-WB2API-Ignored-Tools` 告知；这不等于真的联网搜索或生成图片。强制调用仍会报错。需要这类能力时使用客户端实际提供的函数工具。
- **ClaudeCode 中转**：仅兼容 messages 请求形状，不提供完整 Anthropic 官方平台能力。nf 的“保留全部思考”请求已经支持；它不删除历史。原生服务端搜索、精确计数和其它上下文编辑仍不支持。
- **Gemini 兼容**：运输协议选择 **generate content**。`interactions` 是另一套协议，网关会明确拒绝，不会暗中改道。nf 普通 Agent 使用 SSE；辅助的短生成也支持非流式调用。

## 完成一次可核对的接入

1. 查询模型目录：OpenAI/Anthropic 使用 `/v1/models`，Gemini 使用 `/v1beta/models`。权限与调用密钥的模型白名单一致。
2. 短模型测试通过后，再在自己的测试目录完成一次读文件、调用工具、返回结果与继续对话。短测试的输出预算、思考参数和历史长度与正式 Agent 不相同。
3. 如果失败，保存响应 `X-Request-ID`，在站点日志按 `rid` 查找。区分 400 参数不支持、上游错误和已开始流内的失败事件；HTTP 200 不代表整轮一定成功。
4. 用管理台原始账本核对消费。nf 显示的上下文比例和用量不是网关的独立计费依据。

## nf 0.7.7 的已知边界

- Responses 模式对缺少原生密文的推理历史，可能改写为普通 assistant 文本；Chat 模式会保留独立 `reasoning_content`。网关不能从普通正文中可靠猜回推理边界。这是模式选择时需要考虑的差异。
- nf 部分解析器一看到参数是完整 JSON 就提前判定工具完成。网关现在先验证整组身份、参数和约束，再交付工具，避免无效工具提前执行；不据此宣称 nf 能安全处理任意第三方流。
- Responses 的 done-only 参数、refusal 显示，以及 Chat/Anthropic 的意外 EOF 检测仍有客户端差异。网关正常工具流提供完整参数 delta 和真实终态；拒答、失败或无正文需要检查原始事件。
- nf Anthropic 用量合并可能忽略合法的零修正；Gemini 展示对候选输出和思考的取值也可能不同。网关不会为修正客户端显示而放大、伪造或重复相加 token。
- 本轮已做提取后的原 nf 解析函数重放、真实本地 Handler 与假上游、固定 SDK 等有界验证。这不是对完整 nf GUI 或所有真实模型的验收，也不是百万上下文压缩测试。

更完整的能力、用量和限制见 [agent 接入](agent-compatibility.md)、[Anthropic Messages](anthropic-messages.md) 与 [Gemini generateContent](gemini.md)。
