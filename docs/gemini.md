# Gemini generateContent 兼容说明

v2.3.0 增加 Gemini 的生成与模型发现入口，底层仍复用现有 chat 调度、密钥权限、会话隔离和原始用量账本。它是协议适配，不会把所选 WorkBuddy 模型变成原生 Google 模型。

## 地址与认证

| 接口 | 用途 |
|---|---|
| `POST /v1beta/models/{模型}:generateContent` | 非流式生成 |
| `POST /v1beta/models/{模型}:streamGenerateContent?alt=sse` | SSE 生成 |
| `GET /v1beta/models`、`GET /v1beta/models/{模型}` | 权限内模型列表与详情；列表支持分页 |
| `/v1/models/{模型}:generateContent`、`:streamGenerateContent` | 同功能 v1 别名 |

优先使用 `/v1beta`，避免与 OpenAI 的 `/v1/models` 发现格式混淆。模型 id 保留 `cn:` / `global:`；URL 中可按组件编码。nf 的 Base URL 应填到 `/v1beta`。

支持显式 `Authorization: Bearer`、`x-goog-api-key` 和 `?key=`，按此顺序取值。前一个头存在时，错误值不会由后一个有效值覆盖；重复查询密钥或重复认证头拒绝。查询密钥进入内部请求前移除；日常配置优先使用请求头，避免把密钥放进网址。

畸形查询编码、重复 `alt` 或不支持的运输选项在调用上游前拒绝。流式路径只使用 SSE，不因内部收到一次 JSON 结果就改变客户端请求的运输方式。

鉴权先于 gzip/zstd 解码，复用压缩体、解压后体积限制与模型白名单。错误采用 Google 风格的 `error.code/status/message`。已开始的 SSE 保留错误数据，并附 `finishReason=OTHER` 的失败终态，避免只读取原生候选字段的客户端丢失失败状态。

固定验证的 `@google/genai 2.24.0` 不会因 HTTP 200 流中的错误自动抛异常，还会过滤错误消息细节；调用方需要检查返回的 `finishReason`。网关保留原始 `error` 和 `finishMessage`，不会把错误写进模型正文或补发 `STOP`。

## 支持的请求

| 内容 | 范围 |
|---|---|
| `contents`、`systemInstruction` | user/model 文本、系统文本；保持历史和工具结果顺序 |
| `inlineData` | user 图片；PNG、JPEG、GIF、WebP 的有效 base64；`image/jpg` 规范为 JPEG，仍受图片预算与模型能力限制 |
| `functionDeclarations` | 函数名称、说明、参数 schema；无参声明可省略 parameters |
| `functionCall` / `functionResponse` | 完整对象往返；无 id 的历史按函数名与待配对顺序匹配，孤立、重复或缺失结果拒绝 |
| 工具结果 `response` | 完整业务 JSON，不按内部 `type` 字段误当媒体块，也不截断附加字段 |
| `toolConfig` | `AUTO`、`NONE`、`ANY`；`ANY` 可用 `allowedFunctionNames` 限制已声明函数 |
| 常用生成参数 | `maxOutputTokens`、temperature、topP、topK、seed、stopSequences、presencePenalty、frequencyPenalty；`candidateCount` 只支持 1 |
| 结构化输出 | `responseMimeType=application/json`，可选 responseSchema 或 responseJsonSchema，不能同时指定；转为 JSON/schema 约束并检查实际输出 |
| 思考 | `includeThoughts` 控制展示；thinkingLevel 支持 low/medium/high，以及官方 SDK 的大写枚举；thinkingBudget 支持 0 或 -1，不与 level 混用 |

函数参数 schema 激活公共严格校验；普通合法 JSON 但字段类型错误的工具也不能执行。schema 只支持可忠实转换与编译的部分，外部引用及超过 64 层的嵌套拒绝，不能据此宣称覆盖全部 Google 扩展。

思考预算 0 请求关闭思考并隐藏 thoughts；-1 留给上游默认策略。正数精确预算无法由该上游保证，返回明确错误，不转换成一个虚构的精确 token 上限。level 的实际效果仍由模型决定。

## 流式工具与保活

工具身份和参数在整组校验前不会交付。每个 `functionCall` 一次发出完整参数，不把参数碎片当成多次调用，也不在最终帧重复发送工具。正文与允许展示的思考继续流式输出。

缓冲期间根据实际进展发送可被 nf 解析的空 JSON `data: {}` 保活；它没有工具、usage 或 finishReason，也不计作实际模型内容。请求意外结束仍然失败。工具参数与单个 Gemini 输出事件分别受 4 MiB 限制，适配器缓冲上限为 16 MiB；这些是字节限制，不是模型 token 窗口。

## 用量

假设上游实际报告总输入 P、总输出 C、缓存输入 K、思考输出 R：

- `promptTokenCount=P`，缓存 K 已包含在 P 中。
- 已知有效 R 时，`candidatesTokenCount=C-R`、`thoughtsTokenCount=R`。
- 总输入/输出均已知时，`totalTokenCount=P+C`；缓存和思考不能再加一次。

上游缺少 R 时不能把 R 编造为零。为兼容 nf 读取输出的方式，`candidatesTokenCount` 保留已知聚合输出 C，同时在 `usageMetadata.gatewayUsage` 标明 `thoughtsReported=false`、`candidatesIncludeThoughts=true`、原始 `outputTokenCount` 与完整性。需要候选文本的精确细分时必须检查该扩展，不能把聚合值当作已知的纯正文用量。

缺失或矛盾计量保持可识别，失败和取消保留已知消费；最终网络写出失败在所有适配器收尾后、记账前确认。管理台账本采用响应翻译前的原始观测，不按客户端显示反算或乘固定倍率。

## 明确不支持

Interactions、精确 countTokens、cachedContent、Google 服务端搜索/代码执行、Files/fileData、音频/视频、非空原生 thoughtSignature、多个候选、无法执行的生成选项不作为已支持能力。它们会明确报错；不会暗中换运输协议、改模型名、丢掉签名或删除会话历史后返回成功。

最小配置和 nf 自身差异见 [六模式指南](narrafork.md)。本轮的协议回归、原 nf 解析器重放和本地假上游不等于真实 Google 服务、完整 nf GUI 或生产模型验收。
