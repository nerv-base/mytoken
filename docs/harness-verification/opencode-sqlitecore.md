# OpenCode 只读复审缺陷修复验证（internal/harness/opencode + sqlitecore）

本文记录最终只读复审在 OpenCode/sqlitecore 上**实际复现**的三个缺陷的修复过程与证据。修复范围只含
`internal/harness/opencode/**`（含共享深模块 `internal/harness/opencode/sqlitecore`）与本文档 + `docs/harness-verification/mimo-code.md`
的交叉引用段；`internal/scan`、`internal/store`、其他 harness 一行未动，未提交、未推送，全部数据为合成 fixture（`t.TempDir`），
未读取任何真实用户数据。

严格 RED→GREEN→REFACTOR：先把 `/tmp/mt-review` 中三个对应 probe 的最小断言移入**正式测试**
（`internal/harness/opencode/*_test.go`、`internal/harness/opencode/sqlitecore/sqlitecore_test.go`），
在未改实现的情况下证明 RED，再改实现到 GREEN，最后复核实现与注释。

---

## 1 缺陷一：`session_id` 为 NULL/空 → 整批被拒 → 游标永不前进

### 1.1 RED（改实现之前，原文）

```console
$ go test -count=1 -run 'TestParseFallsBackToThePayloadSession' -v ./internal/harness/opencode/
=== RUN   TestParseFallsBackToThePayloadSession
    opencode_test.go:1053: opencode:sm_camel SessionID = "", want "ses_camel" (the payload states the session the column lost)
    opencode_test.go:1056: opencode:sm_camel ProjectPath = "/Users/dev/fallback", want "/Users/dev/camel" (the recovered session must resolve the session row)
    opencode_test.go:1053: opencode:sm_snake SessionID = "", want "ses_snake" (the payload states the session the column lost)
    opencode_test.go:1056: opencode:sm_snake ProjectPath = "/Users/dev/fallback", want "/Users/dev/snake" (the recovered session must resolve the session row)
    opencode_test.go:1053: opencode:m_camel SessionID = "", want "ses_camel" (the payload states the session the column lost)
    opencode_test.go:1056: opencode:m_camel ProjectPath = "/Users/dev/fallback", want "/Users/dev/camel" (the recovered session must resolve the session row)
    opencode_test.go:1063: event {Harness:opencode DedupKey:opencode:sm_camel SessionID: ParentID: ProjectPath:/Users/dev/fallback ...} has no session or dedup key: the store would reject the whole batch and freeze every later scan
    opencode_test.go:1063: event {... DedupKey:opencode:m_camel SessionID: ...} has no session or dedup key: ...
    opencode_test.go:1063: event {... DedupKey:opencode:sm_snake SessionID: ...} has no session or dedup key: ...
    opencode_test.go:1063: event {... DedupKey:opencode:sm_orphan SessionID: ...} has no session or dedup key: ...
    opencode_test.go:1063: event {... DedupKey:opencode:m_orphan SessionID: ...} has no session or dedup key: ...
    opencode_test.go:1067: got 7 events, want 5: [opencode:sm_camel opencode:m_camel opencode:sm_snake opencode:sm_orphan opencode:m_orphan opencode:sm_clean opencode:m_clean]
--- FAIL: TestParseFallsBackToThePayloadSession (0.01s)
FAIL	github.com/zzstar101/mytoken/internal/harness/opencode	0.457s

$ go test -count=1 -run 'TestIngest' -v ./internal/harness/opencode/
=== RUN   TestIngestSurvivesANULLSessionIDColumn
    ingest_test.go:97: pass 1: Scan: invalid event: harness/session/dedup key
--- FAIL: TestIngestSurvivesANULLSessionIDColumn (0.03s)
=== RUN   TestIngestSurvivesAnEmptySessionIDColumn
    ingest_test.go:124: pass 1: Scan: invalid event: harness/session/dedup key
--- FAIL: TestIngestSurvivesAnEmptySessionIDColumn (0.03s)
```

### 1.2 机制

`internal/store/store.go:480` 对**每一条**事件做校验：

```go
if e.DedupKey == "" || e.SessionID == "" || e.Harness != h {
    return fmt.Errorf("invalid event: harness/session/dedup key")
}
```

于是「一条 row 没有 session」= 「整批 `Write` 失败」= 「cursor 不前进」= 「下一轮重扫同一行、再次失败」。
修复前的调用点（`internal/harness/opencode/opencode.go:434`）只信 `row.SessionID` 列，没有像
`internal/harness/mimocode/mimocode.go:401` 那样回退 payload。

### 1.3 FIX（`internal/harness/opencode/opencode.go:440`）

```go
sid := sqlitecore.FirstNonEmpty(row.SessionID, payload.SessionID, payload.SnakeSessionID)
if sid == "" {
    return
}
acc.Emit(row.ID, sid, payload, sessions[sid], row.Updated, row.Kind == "compaction")
```

- 列优先，`sessionID`/`session_id` 两个 JSON 名字都回退（v1/v2 都走这一条路径）；
- payload 也说不出 session 的行**保守跳过**：它无法归属，`Accumulator.Emit` 本来也会因 `messageID == ""` 或
  `p.Tokens == nil` 之外的原因入账成一条无 session 事件——那正是会把整批打回的形态；而 `Messages` 的水位线在
  `visit` 返回后照样推进（`internal/harness/opencode/sqlitecore/sqlitecore.go:435`），所以后续合法行仍会交付；
- 不在本模块伪造 session id：把 usage 记到一个虚构 session 上是另一种错误数据。

---

## 2 缺陷二：message `id` 为 NULL → `rows.Scan` 失败 → 水位线钉死、尾行永不交付

### 2.1 RED（改实现之前，原文）

```console
$ go test -count=1 -run 'TestMessagesReadsThroughARowWhoseIDIsNull' -v ./internal/harness/opencode/sqlitecore/
=== RUN   TestMessagesReadsThroughARowWhoseIDIsNull
    sqlitecore_test.go:748: pass 1 delivered [a], want [a c]: an id-less row must be skipped, not stall the pass
--- FAIL: TestMessagesReadsThroughARowWhoseIDIsNull (0.01s)

$ go test -count=1 -run 'TestIngestSurvivesANULLMessageID' -v ./internal/harness/opencode/  # （当时与其它 TestIngest 同批运行）
=== RUN   TestIngestSurvivesANULLMessageID
    ingest_test.go:159: ledger holds map[opencode:msg_a:ses_n], want both msg_a and msg_c: the id-less row cannot be counted, but it must not hide the tail
--- FAIL: TestIngestSurvivesANULLMessageID (0.03s)
```

RED 语义：`id 'a'@1000 → NULL@1500 → 'c'@2000` 三行，第 1 轮只交付 `[a]` 并返回**调用者的水位线**
（`{0, ""}`），`c` 永不交付；`rows.Scan` 报错被 `return before` 吞掉（无 error 返回），所以每一轮都卡在同一行。

### 2.2 机制

SQLite 的 `TEXT PRIMARY KEY` 允许 NULL。`Snapshot.Messages` 原来把 `id` 扫进普通 `string`：

```go
if err := rows.Scan(&row.ID, &session, &row.Data, &row.Updated, &row.Kind); err != nil {
    return before   // 驱动无法物化这一行 → fail-closed，返回调用者水位线
}
```

fail-closed 本身是第四轮刻意选择的语义（见 `docs/harness-verification/mimo-code.md` 第四轮 §1/§3），它保住的是
「可恢复的续读点」。但 `id` 与 `time_updated` 不同：`id` 既是 keyset 的续读位置，又是事件的稳定键（dedup key）。
NULL id **不可物化**：`Accumulator.Emit` 在 `messageID == ""` 时直接 return，永远不会有可入账事件，所以为它
fail-closed 保住的不是「一条可恢复的行」，而是「整张表永久停滞」。

### 2.3 FIX（`internal/harness/opencode/sqlitecore/sqlitecore.go:423,434`）

```go
id sql.NullString
...
if err := rows.Scan(&id, &session, &row.Data, &row.Updated, &row.Kind); err != nil {
    return before          // 真正的 Scan 失败仍然 fail-closed
}
row.ID = id.String
row.SessionID = session.String
if row.ID == "" {
    continue               // 既有分支：空/NULL id 都不进账，也不挡后面的行
}
```

选择 `NullString` + 既有跳过分支，而不是在 SQL 里加 `id IS NOT NULL AND id <> ''`：跳过点与现有
`if row.ID == ""` 紧邻，读代码的人一眼能看到「NULL id 走的就是这一条」，而过滤条件会把这一事实藏进查询字符串。
`time_updated`/`data`/`kind` 的 Scan 失败**没有**被改成 `continue`，仍是 `return before`（第四轮语义原样保留）。

### 2.4 覆盖

`TestMessagesReadsThroughARowWhoseIDIsNull` 跑三次：第 1 轮交付 `[a c]` 且水位线 `{2000,c}`；第 2 轮从该水位线
重读交付空、水位线不变（跳过行不会把区间拉回）；第 3 轮（`resetAndRead`）验证 NULL 之后新增的 `d@2500` 仍交付。
`TestIngestSurvivesANULLMessageID` 端到端跑三次，断言 ledger 恰好是 `msg_a` + `msg_c`。

---

## 3 缺陷三：`Num` 把 1e30/Inf/超 int64 饱和成 MaxInt64 → ledger 整数溢出 + 公元数亿年时间

### 3.1 RED（改实现之前，原文）

```console
$ go test -count=1 -run 'TestNumDoesNotSaturateAnOutOfRangeValue|TestEpochMSReadsSecondsMillisecondsAndNanoseconds' -v ./internal/harness/opencode/sqlitecore/
    sqlitecore_test.go:824: input 1e19 = 9223372036854775807, want 0
    sqlitecore_test.go:824: input "1e19" = 9223372036854775807, want 0
    sqlitecore_test.go:824: input 1e30 = 9223372036854775807, want 0
    sqlitecore_test.go:824: input "1e30" = 9223372036854775807, want 0
    sqlitecore_test.go:824: input 1e300 = 9223372036854775807, want 0
    sqlitecore_test.go:824: input 9223372036854775808 = 9223372036854775807, want 0
    sqlitecore_test.go:824: input 18446744073709551615 = 9223372036854775807, want 0
    sqlitecore_test.go:824: input 1.5e19 = 9223372036854775807, want 0
    sqlitecore_test.go:824: input "Inf" = 9223372036854775807, want 0
    sqlitecore_test.go:824: input "Infinity" = 9223372036854775807, want 0
--- FAIL: TestNumDoesNotSaturateAnOutOfRangeValue (0.00s)
    sqlitecore_test.go:859: EpochMS(1742297010000000000) = 55213164-05-08 18:40:00 +0000 UTC, want 2025-03-18 11:23:30 +0000 UTC
    sqlitecore_test.go:859: EpochMS(999999999999) = 33658-09-27 01:46:39 +0000 UTC, want 0001-01-01 00:00:00 +0000 UTC
    sqlitecore_test.go:859: EpochMS(9223372036854775807) = 292278994-08-17 07:12:55.807 +0000 UTC, want 0001-01-01 00:00:00 +0000 UTC
--- FAIL: TestEpochMSReadsSecondsMillisecondsAndNanoseconds (0.00s)

$ go test -count=1 -run 'TestIngestDoesNotStoreASaturatedCounter' -v ./internal/harness/opencode/
=== RUN   TestIngestDoesNotStoreASaturatedCounter
    ingest_test.go:189: SUM(input) over the ledger: SQL logic error: integer overflow (1)
--- FAIL: TestIngestDoesNotStoreASaturatedCounter (0.03s)
```

端到端形态与复审观察一致：两行 `1e30` 饱和成 `9223372036854775807` 进入 ledger，`SELECT SUM(input)` 直接
`SQL logic error: integer overflow (1)`；同一畸形值还把 `time.created` 变成 `292278994-08-17`。底层机制是
`float64 → int64` 在 darwin/arm64 上**饱和**（不是环绕、也不是 0）。

### 3.2 FIX

`internal/harness/opencode/sqlitecore/sqlitecore.go:801,819` —— `Num.UnmarshalJSON` 现在只接受「确实就是它」的 int64：

```go
const numLimit = 9223372036854775808.0   // 2^63：到它就没有有意义的整数转换

if i, err := strconv.ParseInt(s, 10, 64); err == nil {   // 精确整数字面量原样保留
    *n = Num(i)
    return nil
}
f, err := strconv.ParseFloat(s, 64)
if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f >= numLimit || f < -numLimit {
    *n = 0                                               // 溢出/NaN/Inf → 未知（0），绝不饱和成最大值
    return nil
}
*n = Num(f)                                              // 合法小数仍向零截断（1.9→1）
```

- **精确整数优先**是必要的：`9223372036854775807` 的最近 float64 已经是 `2^63`，先 `ParseFloat` 就再也拿不回
  `MaxInt64`；先 `ParseInt` 让合法大整数一位不丢（`1e18`、`MaxInt64` 及其 quoted 形态都被固定测试住）。
- 溢出、NaN、±Inf、下溢到 −2^63 以下一律 0；负数仍照旧由 `Num.Int64()` clamp 到 0（负 token 不参与计数）。
- quoted number 与 JSON number 走同一条路径（先剥引号再解析），两条形态都在测试里。

`internal/harness/opencode/sqlitecore/sqlitecore.go:849,855` —— 时间也只接受**能用**的值：

```go
var plausibleUntil = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

switch {
case v < 1e12:  t = time.Unix(v, 0).UTC()      // 秒
case v < 1e15:  t = time.UnixMilli(v).UTC()    // 毫秒
default:        t = time.Unix(0, v).UTC()      // 纳秒
}
if !t.Before(plausibleUntil) { return time.Time{} }   // 数百年以外不是日期，是坏计数器
```

三级阶梯与仓库里其他 SQLite harness 一致（`internal/harness/zed/zed.go:542` 的 `1e15/1e12/1e9` 阶梯、
`internal/harness/droid/droid.go:786` 的 `1e12` 分界）。超出窗口的值返回 zero，调用点随之回落到该行自己的
`time_updated`（`internal/harness/opencode/sqlitecore/sqlitecore.go:981-983`），所以畸形 `time.created` 不再把
「公元数亿年」写进 ledger，也不会让整行丢掉。

> RED 记录里 `EpochMS(1000000000000)` 一行在 GREEN 阶段被修正：`1e12` 毫秒 = 2001-09-09，本来就在窗口内，
> 断言写错了（`want` 应由「窗口外」改成真正窗口外的 `999999999999999` 毫秒 = 公元 33658）。这是测试自身的
> 修正，不是实现放宽：窗口外仍返回 zero。

---

## 4 与既有契约的关系（未变的部分）

- `TestMessagesKeepsTheCallersWatermarkWhenTheScanIsInterrupted`、`TestMessagesHoldsTheWatermarkWhenARowCannotBeScanned`
  原样通过：`time_updated` 为 TEXT 的行仍 `return before`，可读行照常交付、水位线保持、修好后继续交付。
- `TestMessagesReadsARowWhoseSessionIDIsNull` 原样通过（NULL `session_id` 早就用 `NullString` 读入并交给 payload 回退）。
- `TestTokenClassMapping` 的既有断言全过：input 不因 cache 扣减、负类 clamp、quoted 数字可解、`1.9→1`/`2.7→2` 向零截断。
- mimocode 的 20 条测试全过：它对 `sqlitecore` 的共享只体现在 `Messages`/`Num`/`EpochMS`，这三处的公开语义都只收紧到
  「不会凭空造出最大值」，没有改变合法输入的结果。

---

## 5 残留风险

1. **DB + legacy JSON 同一 message id：事件仍是一条，字段来源不确定（未修，按指示只记录）。**
   `internal/harness/opencode/opencode.go:40-44` 承诺两种来源用同一个 `opencode:<message id>` 去重；但 DB 与
   `storage/message/<ses>/<msg>.json` 是两个**独立 source**，`internal/scan/scanner.go:286-296` 的 worker pool
   并发解析它们，`internal/store` 的 upsert 是 last-write-wins（`internal/store/compact.go:141`），
   所以「最后提交的那个 source 的计数」胜出。实测（新增的
   `TestIngestKeepsOneEventWhenTheDatabaseAndLegacyJSONCollide`，60 次扫描）：

   ```console
   $ go test -count=3 -run TestIngestKeepsOneEvent -v ./internal/harness/opencode/
       ingest_test.go:229: stored counters over 20 scans: database {input:500 output:100} x19, legacy {input:70 output:9} x1 (the winner is completion-order dependent)
       ingest_test.go:229: stored counters over 20 scans: database {input:500 output:100} x20, legacy {input:70 output:9} x0 (the winner is completion-order dependent)
       ingest_test.go:229: stored counters over 20 scans: database {input:500 output:100} x20, legacy {input:70 output:9} x0 (the winner is completion-order dependent)
   ```

   60 次里 59 次 DB 胜、1 次 legacy 胜：**不确定性真实存在**，且**恰好一条事件**（60 次都只有一行
   `opencode:msg_dup`，从未混合两个 source 的字段）。要定义「SQLite 优先」必须改 `internal/scan`（跨 source 排序/合并）
   或 `internal/store`（upsert 合并策略语义），两者都在本次允许范围之外，故不扩大范围：测试只固定可承诺的部分
   （一条事件 + 字段来自单一 source），不确定性记为本残留风险。

2. **NULL/空 `session_id` 且 payload 也无 session 的行被跳过**：该行 usage 不计入 ledger（水位线照样前进，因此不会
   永久卡死）。这是「不伪造归属」与「不漏计」之间的取舍；若上游真写出这种行，症状是静默少计而非停滞，当前没有告警。
3. **`[1e15, 4.1e18)` 的毫秒值会被当作纳秒读**：该区间按毫秒解释是公元 33658 年以后，不可能来自真实记录，
   按纳秒解释才是窗口内的合理解释（1970-01-12 ~ 2100）。这是三级阶梯的固有歧义，与仓库其他 SQLite harness 一致。
4. **`EpochMS` 的 2100 上界是选定的可信窗口**：合法但晚于 2100 的时间戳会被判为不可读并回落到 `time_updated`
   （同样在窗口语义内，不会写进 ledger 一条荒谬时间）。窗口是「只报告能用时间」的显式取舍，不是通用时间解析器。
5. **未做真实用户库抽样**：三个触发形态（`session_id` 可空、`TEXT PRIMARY KEY` 可 NULL、非 STRICT 表可存任意 JSON 数字）
   都来自上游 schema/`database/sql` 类型规则，并已在合成库上端到端复现；仍未对真实用户库做形态统计。

---

## 6 命令与输出（GREEN + 全量验证）

```console
$ gofmt -l internal/harness/opencode          # 无输出
$ go test -count=1 ./internal/harness/opencode/... ./internal/harness/mimocode/...
ok  	github.com/zzstar101/mytoken/internal/harness/opencode	0.818s
ok  	github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore	0.547s
ok  	github.com/zzstar101/mytoken/internal/harness/mimocode	0.744s
$ go test -race -count=1 ./internal/harness/opencode/... ./internal/harness/mimocode/...
ok  	github.com/zzstar101/mytoken/internal/harness/opencode	36.802s
ok  	github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore	2.163s
ok  	github.com/zzstar101/mytoken/internal/harness/mimocode	3.106s
$ go vet ./...                                 # 无输出
$ go test -count=1 ./...                       # 全仓库 60 个包全部 ok，exit 0（无 FAIL）
$ go build ./...                               # 无输出
$ git diff --check                             # 无输出（无空白错误）
```

## 7 新增测试清单

| 测试 | 位置 | 固定住的契约 |
| --- | --- | --- |
| `TestMessagesReadsThroughARowWhoseIDIsNull` | `internal/harness/opencode/sqlitecore/sqlitecore_test.go:733` | NULL id 跳过而非钉死：三轮 pass / 前后行都交付 / 水位线正确 |
| `TestNumDoesNotSaturateAnOutOfRangeValue` | `internal/harness/opencode/sqlitecore/sqlitecore_test.go:783` | 合法大整数精确（含 quoted）；1e19/1e30/1e300/超 int64/Inf/NaN → 0；小数向零截断；负类 clamp |
| `TestEpochMSReadsSecondsMillisecondsAndNanoseconds` | `internal/harness/opencode/sqlitecore/sqlitecore_test.go:837` | 秒/毫秒/纳秒同解；0/负/窗口外/MaxInt64 → zero；非零结果必在可信窗口内 |
| `TestParseFallsBackToThePayloadSession` | `internal/harness/opencode/opencode_test.go:994` | v1/v2 × NULL/空列 × `sessionID`/`session_id` 回退；无 session 的 orphan 跳过；干净行交付并解析出 session 的 ProjectPath；增量续读只交新行 |
| `TestIngestSurvivesANULLSessionIDColumn` | `internal/harness/opencode/ingest_test.go:86` | 端到端：NULL 列不再让 `store.Write` 打回整批 |
| `TestIngestSurvivesAnEmptySessionIDColumn` | `internal/harness/opencode/ingest_test.go:114` | 端到端：空字符串（`NOT NULL` 合法值）同样处理 |
| `TestIngestSurvivesANULLMessageID` | `internal/harness/opencode/ingest_test.go:143` | 端到端：NULL id 不吞掉尾行，ledger 恰好 `msg_a`+`msg_c` |
| `TestIngestKeepsOneEventWhenTheDatabaseAndLegacyJSONCollide` | `internal/harness/opencode/ingest_test.go:175` | 同 id DB+legacy：恰好一条事件、字段来自单一 source（不承诺谁胜，见 §5.1） |
| `TestIngestDoesNotStoreASaturatedCounter` | `internal/harness/opencode/ingest_test.go:239` | 端到端：`SUM(input)` 可算且为 0；ledger 时间戳都落在 [2000, 2100) |
