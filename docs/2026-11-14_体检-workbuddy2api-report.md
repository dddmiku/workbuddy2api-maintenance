# workbuddy2api 第三阶段体检报告

日期：2026-11-14；源码根：`C:/Users/dddmiku/Desktop/na/workbuddy2api-git-update/gateway`。
起点：main / `39eb64e`。第二阶段八个未提交文件及其 TTFB 修复均保留，在此基础上叠加本轮修改。

本轮修复九项已确认的问题：诊断凭据泄露、日志输出 mobile、输入长度缺失、重复关闭资源、两处声明头校验错误及测试状态污染。构建、vet、相关包和全包测试均通过。官方版本对照锚定六份既有抓包，其中最新为 2026-10-29 的 V5.9.3；没有将它描述成 2026-11-14 的最新分发版本。没有连接生产，也没有部署、重启、修改线上配置、提交、推送或发版。

范围与授权：[scope.md](../../artifacts/gateway-health-stage3/scope.md)。确定性标记：**Confirmed** 为源码或可复现实测；**Inferred** 为有证据支持的推断；**Assumed** 为尚未验证的条件，并附验证方法。

## 1. 发现的问题清单

下表位置相对于上面的源码根。修复前的行为均通过源码或合成夹具确认，没有读取、展示真实凭据。

| 编号 / 严重度 | 位置 | 现象与证据 | 修复状态 |
|---|---|---|---|
| F01 / P1 | `internal/upstream/channel_dump.go:95,136` | Confirmed：启用 11128 诊断后，鉴权/设备凭据头原样落盘，URL 查询参数亦原样保留。合成标记回归修复前失败。E02、E04。 | 已修复：统一头部脱敏，诊断 URL 删除 userinfo/query/fragment；完整 UID 截短。 |
| F02 / P1 | `internal/upstream/dump_headers.go:52,62` | Confirmed：头部日志只屏蔽两类凭据头，完整账号头及其他凭据头可输出；设备头存在检查还区分大小写。E02、E04。 | 已修复：凭据头不区分大小写脱敏，账号标识截短；保留安全版本、头名与存在性证据。 |
| F03 / P1 | `internal/panel/backfill_trae.go:93` | Confirmed：回填日志直接输出 `info.Mobile`，没有在输出前保证其已掩码。E04。 | 已修复：只记回填完成事件和截短 UID；账号文件回填功能不变。 |
| F04 / P2 | `internal/upstream/client.go:1163,1187`、`official_fields.go:124,142` | Confirmed：原 prepare 链固定传长度 0，growthEvent 缺 inputLength。两份 V5.9.3 抓包分别为 111144、95310；源码提供 UTF-16 文本计数规则。E01、E02、E03、E04。 | 已修复：在 system 扁平化前计数，复用 payload 解码对象；0 与未知值区分。 |
| F05 / P2 | `internal/upstream/idle.go:38` | Confirmed：Once 只保护停止 channel，重复 Close 仍重复取消/关闭底流，后续调用丢首次错误。16 个并发调用夹具修复前失败。E02、E04。 | 已修复：整次清理共用 Once，保存首次错误。 |
| F06 / P2 | `internal/upstream/trae/stream.go:912` | Confirmed：重复 Remote 会话 Close 会重复关闭底流、发送 stop，并丢首次关闭错误。8 个并发调用夹具修复前失败。E02、E04。 | 已修复：关闭和 stop 共用 Once；保留 stop 原有 10s 上限。 |
| F07 / P2 | `internal/upstream/headers.go:272` | Confirmed：裸 suffix 判断会把 `otherworkbuddy.cn` 接受为 workbuddy.cn 子域。E02、E04。 | 已修复：按域名标签边界匹配，合法子域继续兼容。 |
| F08 / P2 | `internal/upstream/headers.go:657` | Confirmed：36 字符、横线位置正确但含非 hex 字符的 ACP ID 被直接放行。E02、E04。 | 已修复：已格式化 UUID 仍校验 hex 内容；合法值保留，非法值走现有生成兜底。 |
| F09 / P3 | `internal/server/logging_test.go:367` | Confirmed：固定测试键写入全局 sessionPrefixes 后不清理，`-count=3` 后两轮出现 `unseen session must report 0, got 546167`。E03、E04。 | 已修复：测试使用局部 tracker；生产缓存逻辑不变。与模型绑定测试合跑 100 次通过。 |

### A. 客户端一致性审计

| 项目 | 证据与结论 | 本轮处理 |
|---|---|---|
| 官方顶层字段 | Confirmed：六份样本均为 model/messages/tools/temperature/max_tokens/stream/stream_options/reasoning_effort/verbosity/extra_vars/reasoning_summary。见 E01。 | 维持补缺策略；不删除调用方明确指定的扩展字段。 |
| growthEvent model | Confirmed：空 modelName 并不会令正常请求的模型为空，注入器优先使用 body.model；本轮传入 budget.model 兜底。`official_fields.go:83`、`client.go:1187`。 | 与最终 body.model 保持一致；不是独立的空模型缺陷。 |
| growthEvent inputLength | Confirmed：V5.9.3 两份样本的非 system 文本之和恰为 111144、95310。旧样本后续轮次增加消息时仍保留最初的 48742、56849，故它不是每次 HTTP body 长度。官方源码 `TelemetryUtils.getInputLengthFromItem` 使用字符串 length 或各 part.text.length。 | 按 UTF-16 code units 计文本；排除 system/developer、图片 URL/base64 和 tool_calls 结构。已给 extra_vars 原样保留；内部重试复用 prepared，不重复生成事件。 |
| 改写顺序 | Confirmed：计数在 payload 后、FlattenSystemIntoUser 前；随后补官方字段、注入 cache key、压缩图片。`payload.go:59`、`client.go:1163-1193`。cache/image 改写保留其他字段，完整管线回归保留四个官方字段和思考/缓存字段。 | 不把扁平化搬进去的 system 当作新的用户输入；不增加一次大 JSON 解码。 |
| prompt_cache_key | Confirmed：六份官方抓包均没有；网关自动注入，且保留显式值。`cache_key.go:40`。其格式也含网关前缀。Inferred：这构成结构差异；没有证据证明它触发上游拒绝。 | 保留缓存功能，不擅自改变缓存键或清空既有缓存。 |
| thinking | Confirmed：六份抓包没有；但官方源码存在 DeepSeek 条件注入分支，故“官方全源码 0 处存在”这一文件头说法不能作为当前结论。源码副本 `codebuddy-lite-wb.mjs:1016`；网关 `thinking.go` 仅在 DeepSeek 条件下注入、尊重显式 disabled。 | 保留已验证的思考开关；不根据六份抓包缺字段就全局删除。 |
| UA / SDK | Confirmed：V5.9.3 两域分别为 WorkBuddy/5.9.3 + WorkBuddy/5.9.3 或 workbuddy-ai/5.9.3 + CLI/2.165.0；SDK 6.25.0、Node v22.21.1。与 `headers.go:37,46,93,687` 一致。 | 没有更新为未经验证的版本。 |
| X-Domain / 声明头 | Confirmed：标准 CN/global 的 X-Domain、X-Private-Data、X-IDE-*、X-Agent-Purpose 与 V5.9.3 样本一致。官方也发 X-Product=SaaS，它不是网关自报头。`headers.go:263,578,627`。 | 修复域名 suffix 边界；保留配置覆盖。 |
| chat 删除清单 | Confirmed：合法个人账号的 chat 业务头集合由 outbound_fidelity_test.go 双域锁定；Origin/Referer/Machine/Session/Accept-Language/Gateway-Request-ID 被删除，X-Requested-With 保留且官方存在。`headers.go:231,335`。 | 没有按“看起来像浏览器头”猜测新增删除项。 |
| SDK retry-count | Confirmed：六份样本均为 0；网关每次构造新请求也为 0。`headers.go:692`。Inferred：应用层换号/改写重发与 SDK 对同一调用的内部重试不是同一种计数。 | 不盲目递增；需官方失败/重试抓包后再定。 |
| Machine / Session | Confirmed：chat 会删除这两个账号派生头；CommonHeaders 的 refresh/models 保留；BillingHeaders 本身不注入。`headers.go:181,231,737,765`。 | 非 chat 官方当前集合缺少同版本证据，未交换或删除这些头。 |
| Device Token | Confirmed：六份 chat 样本均携带；网关按账号/config/file 取值，全部缺失则省略，出站集合测试明确豁免缺失。`headers.go:131,146`、`outbound_fidelity_test.go:70`。 | 不生成虚假 token，也不把“豁免测试”当作上游接受缺头的实测证据。 |

### B. 其他体检结果

- **并发和资源**：WorkBuddy 的短 RPC 与 chat 共用 Transport，chat 没有总时长限制，但拨号/TLS/响应头分别有界；Trae 复用连接池和第二阶段静默心跳保持。两个新 Once 修复只收敛重复清理。`upstream/client.go:1063`、`upstream/trae/stream.go:1030`、E02。
- **一次性 channel**：FetchModels 两个生产者各发一次到容量 1 的 channel，调用方消费两路；没有循环生产者等待 channel 关闭的问题。RPC 受 Client.HTTP 总超时约束。`client.go:1803`、`global_models.go:194`。旧目录接口没有调用方 context，取消粒度列入第 3 节。
- **错误分类**：11128 渠道拒绝、11140 账号故障/限流、11134 提供方暂时不可用有独立判据。11128 的轮转与解绑是当前策略；11134 不喂账号熔断。对应 `client.go:424,443`、`handler.go:2034,2631`，现有 channel_rejection/provider_unavailable 测试通过。没有修改这些判据，也没有把它们重标成同一种错误。
- **提示词**：defaultprompt 只在 custom 模式使用；ActNote 在带工具请求中统一注入，已有 off 开关。Degraded 常量和状态机仍存在，但当前 handler 无生产调用去替换为 Degraded。`prompt/prompt.go:59`、`server/handler.go:1800,1816`、`server/responses.go:146`。没有扩大触发条件。
- **入口一致性**：Gemini、Messages、Responses 最终均复用 chatCompletions，因此共用本轮出站字段和头修复；各 writer 负责不同下游协议。`gemini.go:76`、`messages.go:63`、`responses.go:1154`。
- **现有契约覆盖**：contract_fidelity 覆盖 reasoning、显式 cache key、schema、错误/length 终态；modern_client 覆盖现代 Codex 入口接受、流式完成、能力拒绝；codex_capture 覆盖合成的多类输入保真。它们不能证明官方当前请求完全一致。`contract_fidelity_test.go:15`、`modern_client_test.go:44`、`codex_capture_test.go:205`。本轮新增真实 prepare 链、脱敏和并发关闭回归补足缺口。
- **默认值**：Schedule.Normalize 已从 DefaultSchedule 取默认；activity count 的“缺省 5”和“显式非正 1”是兼容语义。GlobalEnabled 在 New 的零值和配置层不同，但 server/activity/credit/signin/trial 入口均明确注入开关，未发现这些入口存在默认路由漂移。`config/schedule.go:107,139`、`cmd/server/main.go:227`、`cmd/activity/main.go:168`。

## 2. 已修复的改动

| 文件 / 行 | 改法 | 验证方式 |
|---|---|---|
| `upstream/dump_headers.go:62`、`channel_dump.go:95,136` | 复用不区分大小写的诊断头脱敏；完整 UID 截短；仅诊断副本剔除 URL 鉴权部分，不修改真实请求。改名失败时清理自身临时文件。 | `TestDiagnosticHeadersHideCredentials`、`TestChannelDumpHidesCredentials`；合成凭据标记修复前失败、修复后通过。 |
| `panel/backfill_trae.go:93` | 删除日志中的 mobile 值。 | 源码数据流复核与 panel 包测试；真实文件回填逻辑未修改。 |
| `upstream/payload.go:59`、`client.go:1163,1187`、`official_fields.go:142` | 共用已解码对象计 UTF-16 输入文本，扁平化前取值；0 是空文本，负值为未知。 | 完整 prepare 链回归含中英文、非 BMP 字符、part 文本和图片；零/未知边界；E03 合成基准。 |
| `upstream/idle.go:38`、`trae/stream.go:912` | 取消/关闭/stop 与首个错误结果共用 Once。 | 16 个并发 idle Close、8 个并发 Remote Close；第二阶段真实 HTTP 静默与取消回归仍通过。 |
| `upstream/headers.go:272,657` | 标签边界匹配和 UUID hex 校验。 | suffix lookalike / 非 hex UUID 负例，合法子域兼容正例。 |
| `server/logging_test.go:367` | 测试改用局部 tracker。 | 与 TestKeyModelBindingIsExactFullName 合跑 100 次：7.800s，通过。 |

## 3. 未修复 / 待确认

| 项目 | 原因与建议 |
|---|---|
| 2026-11-14 最新官方分发版本 | Assumed：它仍与 V5.9.3 一致。最新实证是 2026-10-29；旧 asar 为 5.8.3，不能作为更新来源。验证方法：读取当前安装/分发 manifest，再取同版本两域 chat 与短 RPC 脱敏抓包。 |
| root send / RunState 的 inputLength | Inferred：网关 messages 数组可对应官方输入数组；网关没有 CLI RunState 全部语义。已给 growthEvent 保留，内部重试不重算；跨 HTTP 工具轮次若要固定同一 root event，需要调用方提供原始元数据。验证方法：对照同一会话首轮、工具轮次和新用户发送。 |
| 删除 cache key / thinking / store 等扩展字段 | Confirmed：部分字段集合与六份样本不同。Assumed：差异本身会导致拒绝；尚无因果证据。验证方法：同账号、同模型、同正文有限对照，同时核对缓存计费和 reasoning；显式 schema/parallel_tool_calls 等功能不能仅因官方 UI 不使用而丢弃。 |
| SDK 重试计数 | 官方成功样本没有覆盖 SDK 原生重试。验证方法：捕获 SDK 自身重试与应用层重发，分别观察计数；不直接按网关 attempt 增加。 |
| billing/refresh/models 头集合 | 缺同版本、同端点证据；单段 billing UA 来源为旧 banner 业务层。验证方法：分别采集这三类端点，确认 Machine/Session/Device 头及 UA。 |
| 私有上游 Host / X-Domain | 标准域已锁定；自定义 ChatBase 与账号域组合未必同义。验证方法：核对私有部署要求的虚拟域声明，未直接改成 req.URL.Hostname。 |
| ACP 连接标识生命周期 | 代码按 gateway request 生成；官方源码说明进程/连接级，当前尚无完整生命周期证据。验证方法：同进程多会话、重启、跨账号比较 ID 的哈希，不输出原值。 |
| 缺 Device Token 的真实行为 | 省略是当前降级策略，六份官方样本都有该头。验证方法：以同一有效账号做 presence 对照；不伪造 token。 |
| 提示词与角色结构 | Confirmed：ActNote、静态 system 句与搬入 user 的标记均不同于官方样本；system→user 也改变角色优先级，不能宣称语义完全等价。现有处理有渠道兼容历史背景，未回滚。验证方法：在已授权范围对照官方动态首句与工具执行效果；ActNote 可用既有 off 开关对照。 |
| 私有正文 / 上游错误原文 | 11128 诊断保留正文用于回放；其他日志也存在上游原文出口。Assumed：正文中含凭据/个人信息时存在额外隐私问题。验证方法：只用合成标记验证回显链路，再确定正文脱敏策略；本轮未开启生产 dump、未展示原始抓包，亦不声称任意正文均已脱敏。 |
| 目录取消 / 诊断保留上限 | FetchModels 旧接口无调用方 context，但 RPC 有总超时；诊断样本按时间积累，默认关闭。验证方法：取消冷目录请求，测完成时间；确认保留策略后再处理旧样本，不擅自删除用户文件。 |
| 模型绑定原始偶发失败 | 本轮未复现，缺少当次 stderr。100 次合跑与全包均通过；不把不同测试的缓存污染当作它的根因。 |

## 4. 验证结果

完整输出：[verification.txt](../../artifacts/gateway-health-stage3/verification.txt)；可复跑脚本：[verify.ps1](../../artifacts/gateway-health-stage3/verify.ps1)。Go 为 1.26.8 / windows-amd64；临时工作目录位于工作区 artifacts，结束时剩余条目 0。

```text
go build ./...
build: exit 0
go vet ./...
vet: exit 0

go test ./internal/upstream/ ./internal/upstream/trae/ ./internal/server/ ./internal/panel/ ./internal/prompt/ ./internal/pool/ ./internal/config/ -count=1 -timeout=150s
ok workbuddy2api/internal/upstream      3.682s
ok workbuddy2api/internal/upstream/trae 7.600s
ok workbuddy2api/internal/server        8.144s
ok workbuddy2api/internal/panel         3.768s
ok workbuddy2api/internal/prompt        0.028s
ok workbuddy2api/internal/pool          0.907s
ok workbuddy2api/internal/config        0.027s
related: exit 0

go test ./... -count=1 -timeout=180s
ok workbuddy2api/internal/scheduler    24.239s
ok workbuddy2api/internal/server        9.118s
ok workbuddy2api/internal/upstream      4.785s
ok workbuddy2api/internal/upstream/trae 7.658s
all: exit 0
git diff --check: exit 0
```

全包其他结果在完整输出中，以上仅摘录。未运行 race 或真实上游压力/计费对照；PATH 中未发现 semgrep/gosec/staticcheck，本轮使用 Go vet 加人工数据流复核，没有声称完成这些工具的扫描。

合成 ASCII 文本计数基准（10 次，纯计数，不是端到端 TTFB）：128KiB 为 43440 ns/op；2MiB 为 788990 ns/op；两者均 0 B/op、0 allocs/op。完整记录：[benchmark.txt](../../artifacts/gateway-health-stage3/benchmark.txt)。

## 5. 风险与影响面

- growthEvent 的自动生成补齐 inputLength；数据来源是规范化后、system 扁平化前的请求文本。0 和未知值语义已经区分；调用方明确给出的 extra_vars 不覆盖。新增计数复用解码对象，未额外解析大请求。
- 出站请求头只在无效域后缀、非法 ACP ID 上改变；标准两域 UA、SDK、声明头、缓存/思考功能和错误分类保持。
- 诊断输出不再可作为包含鉴权材料的原样报文；需要回放时从既有安全凭据来源读取。原始抓包没有改写或搬移。诊断正文保真边界见第 3 节。
- 重复 Close 的后续调用现在返回首次关闭错误，并且不重复 stop；这是错误结果一致性的变化，正常单次 Close 不变。
- 手机号回填仍落账号文件，但日志只记完成事件。测试 tracker 修复只影响测试进程。
- 实际生产延迟、上游接受率和“被整体打标”概率未测试；不会把本地字段一致性测试当作线上改善证据。

### Evidence → Finding → Path

**E01**：2026-11-14，file；来源为六份既有官方抓包，经本地脱敏脚本生成 [official-capture-summary.jsonl](../../artifacts/gateway-health-stage3/official-capture-summary.jsonl)。SHA-256：`097fc59dc0f624f0055cce315409ed378e887d71170bb566909185426a0a6525`。每条记录另含原件 hash。原文仅输出字段名、公共版本、角色和长度，不含鉴权值。重现：`python ../artifacts/gateway-health-stage3/audit_safe.py capture <本地原件路径>`；原件不随报告分享。关联 F04 和 A 部分。

**E02**：2026-11-14，command；[verification.txt](../../artifacts/gateway-health-stage3/verification.txt)，SHA-256：`0064db816b424c60c538b17b2fc6d97c130ac1e738f8c5c40bb17dc73e771eca`。重现：在源码根运行 `& ../artifacts/gateway-health-stage3/verify.ps1`。关联 F01—F09；各测试函数位于 health_audit_test.go、trae/remote_close_test.go 及 logging_test.go。

**E03**：2026-11-14，command/file；[benchmark.txt](../../artifacts/gateway-health-stage3/benchmark.txt)。重现命令见文件首行及重复测试命令；只使用合成输入。关联 F04、F09。旧 prefix 用例的 -count=3 失败已在第二阶段记录，本轮用局部状态消除复现。

**E04**：2026-11-14，file；各 Findings 的当前源码位置与本地 diff。重现：`git diff -- internal/upstream internal/panel/backfill_trae.go internal/server/logging_test.go`；新回归文件需单独读取。官方算法辅助参考为 `_rea_wb/cli_only/codebuddy-lite-wb.mjs:2269`，该源码副本未作为当前版本号依据。

**P01 / callflow**：客户端入口 → chatCompletions（E04）→ prepareChatBodyWithInputLength（F04）→ Neutralize/Flatten → OfficialFields/Cache/Image → gzip + ChatHeaders（F07/F08）→ 上游 → idle/Remote Close（F05/F06）。下游协议各自转换，共享出站适配。残余差异见第 3 节。

**P02 / callflow**：11128 或诊断开关 → 诊断副本头部与 URL 脱敏（F01/F02，E02/E04）→ 本地记录；Trae 资料回填 → 写账号文件 → 不输出 mobile 的完成日志（F03）。不存在本轮线上写入路径。

交付自检：源码与授权范围核对完成；自动静态检查与人工复核完成；负例回归、构建、相关包、全包通过；证据摘要与验证输出落盘；第二阶段改动保留；未操作生产和 Git 发布流程。
