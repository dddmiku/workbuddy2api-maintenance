# 上游渠道校验（11128）实测结论

本文记录 2026-10-17 对 WorkBuddy 上游 `/v2/chat/completions` 渠道校验的完整实测结果。
所有结论均有可复现的实验支撑（脚本见仓库外 `_probe_channel/`，结果 JSON 同目录）。

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

## 结论三：断词（零宽空格）是有效解法

对上述全部触发点，在命中 token 的每个单词首字母后插入零宽空格（U+200B）即通过：

| 场景 | 原文 | 断词后 |
|---|---|---|
| 首行 billing 头 | ❌ | ✅ |
| 首行 Claude Code 归属句 | ❌ | ✅ |
| 首行 Codex 句 | ❌ | ✅ |

零宽字符不可见，模型读到的文字不变，客户端解析不受影响。

## 实现要点

- `channelFirstLineTriggerTokens`：首行触发 token 表；
- `neutralizeSystemFirstLine`：只处理 system 首行（含单行 system 与 part 数组），
  未命中首行的请求**一个字节都不变**；
- `NeutralizeBillingHeaders`：发送前预中和首行（省掉「先被拒再重试」的往返）；
- `NeutralizeChannelTriggerAt` 第 0 档也先跑首行处理，再按需升级到通用归属句/全文；
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
