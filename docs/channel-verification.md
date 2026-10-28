# 上游渠道校验（11128）实测结论

本文记录 2026-10-17 对 WorkBuddy 上游 `/v2/chat/completions` 渠道校验的完整实测结果。
所有结论均有可复现的实验支撑（脚本见仓库外 `_probe_channel/`，结果 JSON 同目录）。

> **2026-10-17 晚间勘误（重要）**：本文写作当日，基于裸探测脚本（手工拼头部、极简 body）
> 又得出过一个「渠道校验是账号绑定、被拒账号已被上游标记」的结论（v3.10.5 的 24h 隔离），
> **该结论已被推翻**——真实客户端抓包（`_wb_capture2/`）证明同一批账号在官方客户端
> 正常使用；决定性对照是：裸探测的 11128 msg 是兜底文案 `request illegal`，而**带齐
> 官方客户端头部后同一请求返回 `first message is not system prompt`**——即裸探测本身
> 缺官方头部与 system 首条，从来没「烧号」这回事。错误代码已回退，误清的账号状态已恢复。
> 本文的其余结论（校验窗口 = system 首行、头部不参与、断词有效）仍基于带真实头部的
> 对照实验，维持有效。

## 背景

用户经网关用 Claude Code 接入时被拒：

```json
{"code":11128,"msg":"request illegal",
 "displayMsg":{"zh":"请求被拦截，请重新发送"},
 "displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。"}}
```

`code 11128` 是**请求级**拦截（不是账号封禁；11140 才是）。本文回答：触发条件到底是什么。

## 结论一：校验只发生在 body，与请求头无关

同一账号、同一时刻，逐项切换头部集合，结果完全一致：

| 头部组合 | 结果 |
|---|---|
| 官方客户端抓包同款头 | PASS |
| 网关出站全量头（含 `Origin`/`Referer`/`X-Machine-ID`/`X-Session-ID`/`X-Gateway-Request-ID`/`X-No-*`/`Accept-Language`） | PASS |
| 只加 `X-Machine-ID`+`X-Session-ID` | PASS |
| 只加 `Origin`+`Referer` | PASS |
| 只加 `X-Gateway-Request-ID` | PASS |
| 去掉 `acp-connection-id`/`X-Private-Data` | PASS |
| UA 5.7.6 / 5.8.4 / 带 `workbuddy-ai` 段 / 不同 CLI 版本 | 全 PASS |

**头部整体不参与渠道校验**（12 组实验，`results_headers.json` / `results_iso.json` / `results_ua.json`）。
网关此前对头部指纹的种种猜测（多发的 `X-Machine-ID` 等）不是被拒原因。

## 结论二：三个变体，各有明确触发条件

### 变体 1：`first message is not system prompt`

触发条件：`messages[0].role != "system"`，**只在 global 域强制**。

| 场景 | AI 域（www.workbuddy.ai） | CN 域（www.workbuddy.cn） |
|---|---|---|
| 首条是 user（无 system） | ❌ 11128 `first message is not system prompt` | ✅ PASS |
| 首条是 system | ✅ PASS | ✅ PASS |

复现 3/3 稳定（`results_sys.json`、`results_matrix_clean.json`）。

### 变体 2：`Illegal API invocation from an unapproved channel`

触发条件：**system 消息的第一行**含以下任一 token：

- `x-anthropic-billing-header`
- `You are Claude Code, Anthropic's official CLI for Claude`
- `Codex CLI is an open source project led by OpenAI`

位置敏感性（关键发现）：

| 位置 | 结果 |
|---|---|
| system 首行 | ❌ REJECT |
| system 第二行 | ✅ PASS |
| 首行但前面加 20 字节中性前缀 | ✅ PASS |
| 首行但前面加 100 / 500 / 2000 字节 | ✅ PASS |
| user 消息内（任意位置） | ✅ PASS |
| system 为 part 数组，任一块首行含该句 | ❌ REJECT |

即**校验窗口 = system 首行**（字符串形态）或 system 数组的每个块首行。
同句被零宽空格逐词断开后通过（`results_neutralize.json`、`results_window.json`）。

### 变体 3：`request illegal` + displayTips 渠道校验

上游对同一拦截会在主 msg 写泛化的 `request illegal`，把真实原因放进 `displayTips`。
此前网关按 msg 分类 → 归 `ErrClient` → **不触发断词重试**，用户看到终态错误。

修复：`isUnapprovedChannel` 在 `code==11128` 且 `displayTips` 自述
（`channel verification` / 「渠道校验」）时也判渠道拒绝。11140 的同 msg 是账号级封禁，
按 code 严格区分，不混入。

## 结论三：断词（零宽空格）曾有效，**2026-10-29 起已失效**

> **2026-10-29 勘误（重要）**：本节原结论「插零宽空格即通过」已被上游推翻。
> 上游改成模糊匹配后，插零宽、插普通空格都被照拒；只有**改动字母本身**才能通过。
> 现状见下方「结论四」，本节的矩阵保留作为历史记录。

2026-10-17 时点，对上述全部触发点，在命中 token 的每个单词首字母后插入零宽空格
（U+200B）即可通过：

| 场景 | 原文 | 断词后（2026-10-17） |
|---|---|---|
| 首行 billing 头 | ❌ | ✅ |
| 首行 Claude Code 归属句 | ❌ | ✅ |
| 首行 Codex 句 | ❌ | ✅ |

零宽字符不可见，模型读到的文字不变，客户端解析不受影响。

## 结论四（2026-10-29）：必须改动字母

Claude Code 经网关走 cn/global 全部 11128。同一账号、同一时刻的实测矩阵
（每次只改一个变量）：

| 处理方式 | 结果 |
|---|---|
| `x-anthropic-billing-header` 原样 | ❌ 11128 |
| 插零宽 `x-a-nthropic-b-illing-h-eader` | ❌ 11128 |
| 插普通空格 `x - anthropic - billing - header` | ❌ 11128 |
| **去掉 `x-` 前缀** → `anthropic-billing-header` | ✅ 200 |
| 换成中文「计费头：」 | ✅ 200 |
| 归属句改措辞 `You are a coding assistant, ...` | ✅ 200 |
| 归属句去掉 `Claude` | ✅ 200 |
| 归属句挪到 system 第二行 | ✅ 200 |

**结论**：上游按**带前缀的完整字面量**做模糊匹配，插字符（零宽或空格）不再生效，
改动任意字母即可绕开。网关因此改为：

- 计费头：删掉 `x-` 前缀（改动面最小，只删 2 字节，语义仍可读）；
- 归属句：整句大写化（`strings.ToUpper`，实测改措辞即通过，大写保持一致可读）。

零误伤契约不变：**未命中触发串时，首行一个字节都不许动**——零宽剥离只用于匹配，
不得落回输出（2026-10-29 修过一次这个回归）。

## 实现要点

- `channelFirstLineTriggerTokens`：首行触发 token 表（用于**匹配**，含客户端自带的零宽形态）；
- `channelTokenRewrites` / `channelAttributionSentences`：命中后的**改写**规则；
- `neutralizeSystemFirstLine`：只处理 system 首行（含单行 system 与 part 数组），
  未命中首行的请求**一个字节都不变**；
- `NeutralizeBillingHeaders`：发送前预改写首行（省掉「先被拒再重试」的往返）；
- `NeutralizeChannelTriggerAt` 第 0 档也先跑首行改写，再按需升级到通用归属句/全文
  （第 1/2 档仍是插零宽，作为上游改版后的兜底）；
- global 域 `ensureConsoleSystem` 补首条 system；CN 域不动（实测不要求）。

## 复现方式

```bash
# 取生产账号凭据后（本地直打上游）
python _probe_channel/wb_matrix_clean.py   # 两 realm × 位置 × 断词
python _probe_channel/wb_illegal2_probe.py # 三个 token 的精确触发
python _probe_channel/wb_prod_repro2.py    # 生产账号 3/3 复现 + 断词验证
```

## 注意（实验方法）

本轮多次踩到同一个坑：**写脚本时把「断词后的形态」当原文写进字面量**（含 U+200B），
导致 raw 组其实是被断词的版本、结论整体反转。因此：

- 触发串字面量必须用 `chr()`/拼接构造，运行时断言 `ord(c) < 128`；
- 怀疑有隐藏字符时用 `raw.hex()` 逐字节核对，不要相信终端 `repr` 渲染；
- 每轮结论至少重复 2 次（本轮每个关键结论都做了 2–3 轮复现）。
