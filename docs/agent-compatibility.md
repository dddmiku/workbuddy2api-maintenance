# agent 接入与能力边界

使用管理台创建的调用密钥，先查询 `/v1/models` 和 `/v1/capabilities`，再选择实际可见的完整模型 id。网关兼容多种协议，不替上游模型承诺所有工具或平台能力。

v2.3.0 支持四种协议；nf 的六种界面模式如何填写地址见 [六模式配置](narrafork.md)。

## 入口

| 路径 | 用途与认证 |
|---|---|
| `POST /v1/chat/completions` | OpenAI chat，Bearer |
| `POST /v1/responses` | 无状态 Responses，Bearer；Codex 等客户端使用完整历史续接 |
| `POST /v1/messages` | Anthropic messages 适配，`x-api-key` 或 Bearer |
| `POST /v1beta/models/{模型}:generateContent`、`:streamGenerateContent?alt=sse` | Gemini 生成，`x-goog-api-key`、Bearer 或查询 key；支持 `/v1/models/` 同功能别名 |
| `GET /v1beta/models`、`GET /v1beta/models/{模型}` | Gemini 模型发现，按同一密钥权限过滤 |
| `GET /v1/models`、`GET /v1/models/{model}` | 按密钥白名单返回 OpenAI 模型对象，接受上述两种密钥头 |
| `GET /v1/capabilities` | 本网关支持范围，使用调用密钥认证 |
| `POST /v1/messages/count_tokens` | 当前明确返回不支持，不伪造精确计数 |

显式 Authorization 始终优先，包括其内容无效时；其它密钥来源不能覆盖它。Gemini 中 `x-goog-api-key` 优先于查询 key，查询密钥进入内部请求前移除。模型白名单逐字匹配，`cn:m`、`global:m` 与裸名是不同绑定。详情查询对隐藏和不存在的模型均返回同类 404。

OpenAI 客户端 Base URL 填到 `/v1`，Gemini 填到 `/v1beta`。Anthropic 官方 SDK 自行添加 `/v1/messages`，Base URL 使用站点根路径；nf 只添加 `/messages`，因此 nf 的两个 Anthropic 模式需要填到 `/v1`。网关不依赖修改客户端密钥、模型名或任意缩小窗口才能识别超限。

## 工具与流式契约

- 函数调用和工具结果复用同一执行链；Responses 的 custom/namespace 工具通过函数形式桥接并恢复公开名称。
- 上游工具参数可以分片、晚于名称到达；网关先验证整组工具身份、声明集合、参数对象和输出约束，再向客户端交付工具身份与参数。仅推迟 done 不足以阻止某些客户端提前执行。
- Anthropic 消息重建文本、明文 thinking、图片、工具块、心跳及 usage；有工具的正常完成使用 `tool_use`。客户端必须等工具块完成后执行，start/delta 只是进度。
- JSON schema、强制工具选择和禁止并行等约束经过实际校验；auto/required 也只能调用已声明工具，空工具列表不跳过检查。能否生成所需结果仍取决于所选模型。
- `web_search`、`tool_search`、`image_generation` 等客户端默认可能携带的声明会被兼容接受并过滤，但网关不执行它们。响应通过 `X-WB2API-Ignored-Tools`、Warning 和日志明确说明；强制调用不支持工具会拒绝。

服务端文件检索、MCP、图像生成、计算机工具和原生容器等平台能力没有因此获得实现。Responses 存储、`previous_response_id`、仅含不可解读密文/redacted 的思考历史、不能忠实转换的文档/音频输入也不能假装成功。上游没有原生签名时不伪造思考密文或签名。Anthropic 仅接受明确保留全部 thinking 的无裁剪 context_management；Gemini 支持 generate content，Interactions、Files 和 cachedContent 仍拒绝。

## 用量与压缩

上游实际报告的输入、输出、缓存与扣费是计量依据。缓存属于输入，思考属于输出，不重复相加；缺失字段保持未知，非法迟到明细不会抹掉已有有效值。内部重试合计各次已知消费，但同一个客户端请求只计一次。

网关始终向上游请求 usage。原生 Chat 显式 `stream_options.include_usage=false` 时，只有最终客户端输出隐藏 usage；工具契约校验和账本仍读取完整原始帧。Responses/Messages 保留各自的协议用量结构；Gemini 的未知思考拆分由 `gatewayUsage` 说明，见 [Gemini 用量](gemini.md#用量)。

上下文超限不会通过删除旧轮次、移动图片角色、填入被拒请求体积或乘固定倍率来伪装成功。流式 Responses 用 `response.failed` 和 `context_length_exceeded` 提供真实失败信号，让支持该行为的客户端在后续续接时尝试摘要恢复。当前失败轮仍然失败；恢复时机由客户端版本和配置决定。

没有实现原生 `/responses/compact`，也不会生成伪造的加密压缩条目。模型目录未知的上下文/输出上限省略；已知值保留上游来源。`/v1/models?client_version=...` 仍是普通模型列表，不是 Codex 私有 manifest。没有采用参考项目的硬编码模型窗口和工具截断策略。

Responses 的 `truncation=auto` 与未支持的 `reasoning.context/mode` 明确拒绝，不再接受后忽略或原样回显为已生效。

## 传输与诊断

支持 identity、gzip、zstd 请求体，鉴权先于解压，压缩体与解压后体积都受配置上限约束。SSE 单行及累计事件上限为 64MiB，前置计量观察器与协议解析器都在分配时检查；这不是整段会话或模型 token 窗口。

各适配器还有更小的缓冲边界：Chat/Responses 工具或协议缓冲为 16MiB，Gemini 工具参数和单事件为 4MiB。缓冲期间正文/思考继续流出，并按实际进展发送保活；Gemini 使用空 JSON data 帧，不填假 usage 或终态。

每次真实网络写出和 flush 最多等待 30 秒，写完即清除期限，正常静默推理不沿用该期限。所有适配器从外到内幂等收尾后才记账；客户端停止读取或最终 flush 失败会取消上游、释放租约并记录失败，保留已知消费。

响应 `X-Request-ID` 由网关生成，Anthropic 还提供同值 `Request-ID`；诊断时与请求日志 `rid=` 对应。配置中的图片字节预算与模型上下文不同，开启图片预算时可能省略超预算图片，不应把它当作精确 tokenizer。

## 验证范围

本轮覆盖协议转换、权限、工具终态、原始计量、取消、超限和真实本地 HTTP/2 静默段等回归。正式支持某个 agent/SDK 组合仍需以指定客户端版本、指定模型的真实读/改/测/续接结果验证；不要将短探针或小阈值摘要成功写成百万上下文恢复成功。

架构取舍主要参考 new-api 与 sub2api，第三个同源面板项目只辅助对照；具体版本和构建校验见 [统一架构](architecture-unified.md)。
