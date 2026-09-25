# 请求消费明细

`requestlog` 独立保存已完成请求的有界元数据。它不取代 `usage` 累计账本，也不自行提供或挂载 HTTP 路由。调用方应在最终状态确定后写一次记录，在管理鉴权之后调用查询接口。

## 接口

```go
store, err := requestlog.Open(path, requestlog.Options{})
err = store.Append(record)
page, err := store.List(requestlog.Query{
    KeyID: keyID, Model: fullModel, Status: status, RequestID: requestID,
    Offset: 0, Limit: 50,
})
record, err = store.Get(requestID)
err = store.Close()
```

- `Append` 接收真实网关 `RequestID`、完整模型名、调用密钥的管理 ID/名称和最终状态；`RecordedAt` 由存储分配。正文、请求头、密钥值、cookie、任意上游错误文本没有对应字段。代码字段只允许有界机器码。
- `List` 返回 `Page.Items []Summary`，全部筛选条件按精确匹配组合，按实际写入顺序从新到旧排列。默认每页 50 条，最多 100 条。列表不带尝试/调度数组；`LastDecision` 仅带最后调度的少量事实，账号 ID 摘要超过 64 字节时明确标记截断，详情保留完整有界值。`Total` 是当前筛选结果数，分页属于当前快照；页面翻页时不要自动刷新。
- `Get` 返回 `Record`，包含每次上游尝试和调度事实。返回值与内部缓存完全分离，调用方改动返回对象不会修改存储。
- `Close` 等待已进入的操作完成；没有后台队列或未刷新的缓冲。之后的操作返回 `ErrClosed`。
- 不存在返回 `ErrNotFound`，重复的仍在留存期内的请求 ID 返回 `ErrDuplicate`，输入越界返回 `ErrInvalidRecord`/`ErrRecordTooLarge`/`ErrInvalidQuery`。未知格式或中间帧损坏返回 `ErrCorrupt`，不以旧内存副本覆盖原文件。

## 用量语义

所有尝试的 input/output/cache/reasoning token 和 credit 均使用指针。`nil` 在 JSON 中为 `null`，只代表没有观测；指向 `0` 才是确认的零。`UsageComplete` 只表示 input 和 output 都有上报，可选缓存、推理和 credit 仍然可能缺失。

摘要只累计不同 `Attempt` 已经观测到的数值。部分尝试缺失时，显示的是已知消耗，不能据此断言总消耗或真实账单。各指标的 `Missing*Attempts` 以及 `UnknownAttempts` 保留覆盖情况。没有一次对应观测时，该指标依然为 `null`。

`UpstreamStarted=false` 的记录使用 `UsageNotStarted`，不能写成测得 0。若已调用上游却没有可存的尝试信息，至少计为一次未知尝试。`AttemptCount` 表示实际总次数；可存明细少于它时，`AttemptsTruncated` 自动为真，未存的尝试仍计入所有缺失维度。

`Decision` 只接收账号池在实际锁内捕获的事实。`WeightUnits/WeightTotal` 仅供真实抽签解释；粘性、探索或兜底路径不推算随机概率。`CostPer1K` 是历史观测的每千 token credit 单价，结合 `CostState/CostObservedAt/CostSamples/CostUnknownReason` 解释，并用 `CostUsedForSelection` 标出本次是否采用该观测。它不是此次消费 credit。普通排除计数与 `fallback_*` 排除计数属于独立扫描，不能相加当成唯一账号数。

`Record`、`Attempt`、`Summary` 的可选 `FinishReason` 保留真实结束机器码。例如 HTTP 200 且 `finish_reason=length` 仍然可能达到输出上限，不能只据 HTTP 状态把它解释为无限制完成。未观测到时保留空值。`TTFBMS` 使用可空指针区分未观测首字节与确认为 0 ms。

## 留存与输入上限

| 项目 | 默认或硬上限 |
| --- | --- |
| 留存条数 | 10000 |
| 留存时间 | 7 天，按存储写入时间 |
| 活动日志文件 | 64 MiB |
| 单条已编码帧 | 64 KiB |
| 单条尝试明细 / 调度记录 | 各 64 条 |
| 实际尝试总数 | 4096 |
| 每个调度计数 map | 32 项 |
| 请求/密钥 ID | 128 字节 |
| 模型、名称、上游账号 ID | 256 字节有效 UTF-8，拒绝控制字符 |
| 机器码 | 64 字节 |

`Options` 的零值使用默认值；显式值只能收紧上限。共享同一路径的进程必须使用相同配置。打开、有效写入及查询时执行留存，不在空闲时启动后台清理线程。

达到条数或容量上限后，按写入顺序批量释放约 10% 的空间，降低长期运行中每条请求重写整个文件的开销。因此保留数量可以略少于上限。过期明细不会出现在任何列表或详情里。

原子压缩需要一个临时快照，峰值磁盘空间可额外使用一个活动文件的容量；进程下次打开时，仅清理这个确切日志路径所属的遗留临时文件。累计账本不因明细留存而减少。

## 持久化约定

日志首行记录格式版本、随机 generation 和尾部恢复计数。后续每行是带 CRC32 的记录帧，以换行作为完整帧结束标记。

每次读、追加和压缩都先取得 `<path>.lock` 的跨进程排他锁，再打开当前文件。文件锁在稳定的旁路文件上，数据文件可以原子替换；不会持有已经被替换的旧数据文件句柄。相同 generation 可以增量读取，发生压缩后会重新加载；任何整体替换之前都完整校验当前文件。

追加成功前同步文件；压缩先写完并同步同目录临时文件，再原子替换。Unix 同步所在目录。遇到 I/O 错误，提交状态可能不确定：调用方应查询同一 ID 或重试相同 ID，不能另造 ID 重新记一次。

仅末尾损坏或未写完的帧允许自动删除。此前有效帧保留，并在 `Page.Recovery` 返回累计恢复次数、舍弃尾部字节数和最近恢复时间。这些信息不包含舍弃内容。中间损坏、重复 ID、未知版本和超大文件报错并保留原文件。

新目录使用 `0700`，日志、锁和临时文件使用 `0600`；拒绝末级符号链接、硬链接和特殊文件。存储路径及其上级目录必须由管理员配置和控制。Windows 不提供 POSIX 权限语义，实际访问权限继承所在目录的 ACL；应把数据目录限制为网关账户可访问。

## 验证

`go test ./internal/requestlog` 包含真实子进程写入测试、旧进程跨压缩续写、使用量缺失与零值、紧凑列表、留存、损坏恢复及权限边界测试。`go vet ./internal/requestlog` 检查包内静态问题。有可用 CGO 工具链时运行 `go test -race ./internal/requestlog`。

测试仅使用临时目录和合成记录，不连接真实上游、不读取生产密钥。对外路由鉴权、真实请求完成时的接线及管理台行为由集成测试另行覆盖。
