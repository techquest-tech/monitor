# Monitor

Monitor 是一个基于 Go 语言的模块化监控和链路追踪库，主要设计用于集成到使用 Gin 框架的 Web 应用中。

它的核心设计理念是**“一次采集，多端分发”**。通过定义统一的监控接口，它能够捕捉系统的 HTTP 请求、错误日志以及定时任务执行记录，并将这些数据分发到多个不同的后端存储或分析系统中。

## 编译构建 (Build & Tags)

本项目采用 **Go Build Tags** 来按需编译和启用不同的监控后端。这意味着你必须在编译时通过 `-tags` 参数来指定需要集成的模块，否则默认情况下不会启用任何后端。

### 可用 Build Tags

| Tag 名称 | 描述 | 包含的模块 |
| :--- | :--- | :--- |
| `monitor_default` | **推荐**。启用常用的云原生监控组合。 | Loki, Azure Insights, DataPool (Parquet) |
| `monitor_loki` | 仅启用 Grafana Loki 支持。 | Loki |
| `monitor_insights` | 仅启用 Azure Application Insights 支持。 | Azure Insights |
| `monitor_datapool` | 仅启用本地 Parquet 文件存储支持。 | DataPool |
| `monitor_db` | 启用关系型数据库存储支持 (GORM)。 | Database |
| `monitor_duckdb` | 启用 DuckDB 落库支持（Quack 远程协议，批量写入）。 | DuckDB |
| `monitor_mqtt` | 启用 MQTT 订阅源，将消息转换为 `TracingDetails`。 | MQTT Source |
| `monitor_messaging` | 启用消息队列桥接模式 (Redis/EventBus)。<br>**注意**: 仅在未启用 `monitor_default` 时生效。 | Messaging Bridge |

### 编译示例

**1. 启用默认组合 (推荐)**
适用于大多数云原生部署场景，同时支持日志分析、APM 和数据归档。
```bash
go build -tags monitor_default .
```

**2. 仅启用 Loki**
适用于只需要日志聚合的轻量级场景。
```bash
go build -tags monitor_loki .
```

**3. 启用数据库存储**
如果你需要将监控数据直接写入 MySQL/PostgreSQL。
```bash
go build -tags monitor_db .
```

**4. 混合使用**
你可以组合多个 tag 来满足特定需求。
```bash
# 同时启用数据库和 Loki
go build -tags "monitor_db monitor_loki" .
```

## 核心架构 (Core Architecture)

项目围绕 `MonitorService` 接口展开，该接口定义了三种核心监控行为：

*   **ReportTracing**: 上报链路追踪数据（HTTP 请求/响应详情）。
*   **ReportError**: 上报系统错误。
*   **ReportScheduleJob**: 上报定时任务（Cron Job）的执行历史。

它使用了一种**适配器（Adaptor）模式**或**发布-订阅模式**。核心代码（如 Gin 中间件）产生监控数据后，通过 `TracingAdaptor` 等通道分发给所有注册的 `MonitorService` 实现。这意味着你可以同时将日志发送到 Loki、数据库和 Azure，而无需修改业务代码。

## 数据流程 (Data Flows)

### 总体数据流

监控数据遵循「**采集 → 全局 Adaptor 分发 → 后端持久化**」三段式流程，全程解耦：

```
采集源 (Instrumentation)                         全局 Adaptor (可替换)              后端 (Backends)
──────────────────────────                     ──────────────────────          ──────────────────────
Gin 中间件        ── TracingDetails ──┐                                        ┌─→ Loki     (同步推送 + 自适应限速)
HTTP 出站拦截     ── TracingDetails ──┼── Push ──► TracingAdaptor     ──订阅──┼─→ Database  (攒批定时落库)
MQTT 订阅         ── TracingDetails ──┘          (chan / Redis Stream)          ├─→ DuckDB   (整批一条 INSERT)
定时任务执行记录   ── JobHistory     ──► schedule.JobHistoryAdaptor              ├─→ Insights (Azure APM)
系统/业务错误     ── ErrorReport    ──► core.ErrorAdaptor                       ├─→ DataPool (Parquet)
                                                                                └─→ Console  (调试)
```

*   **采集**：Gin 中间件拦截入站 HTTP、`http.RoundTripper` 包装器拦截出站 HTTP、MQTT 订阅消费消息、`gin-shared` 的定时任务与错误组件上报，统一封装成三类事件。
*   **分发**：三个全局 Adaptor（`TracingAdaptor` / `schedule.JobHistoryAdaptor` / `core.ErrorAdaptor`）默认是进程内 `ChanAdaptor`（缓冲 10000），可被替换为 Redis Streams 实现（见下文「Redis Streams 桥接模式」）。每个后端通过 `SubscribeMonitor` 订阅，支持**逐条**（`Subscripter`）与**批量**（`SubscripterBatch`）两种投递方式。
*   **持久化**：各后端收到数据后按自身策略写入目标存储，彼此独立、互不影响。

### 入站 HTTP 请求追踪流程

`gin.go` 的 `LogfullRequestDetails` 中间件按以下顺序处理每个请求：

1. 记录开始时间；`User-Agent` 以 `kube-probe` 开头的健康检查请求直接放行，不采集。
2. 用 `c.FullPath()` 得到路由模板（如 `/api/user/:id`），再依据 `Included` / `Excluded` 白黑名单判断是否记录该路由。
3. 若开启 `Request` 且命中规则，读取并缓存请求体（`application/x-www-form-urlencoded` 会自动 URL 解码）。
4. 若开启 `Resp` 且命中规则，用 `RespLogging` 包装 `ResponseWriter`，在写响应时缓存响应体。
5. 调用 `c.Next()` 放行下游处理器。
6. 计算耗时、状态码、`TargetID`（`tracingID`）、响应体；通过 `extractTracingUser` 从上下文提取 `Tenant`（owner）与 `Operator`（user）。
7. 组装 `TracingDetails`（含 `VerbosityLevel = VerbosityLevelByMethod(method)`）并 `TracingAdaptor.Push`。

### 出站 HTTP 拦截流程

`requesttracing.go` 的 `Log(operationname)` / `LogOutbound()` 返回包装后的 `http.RoundTripper`：

1. 记录开始时间、方法、URI；读取并缓存请求体（form 同样解码）。
2. 调用底层 `RoundTrip` 执行真实请求。
3. 请求出错（`err != nil`）→ 记录 `Resp = "error:..."` 并 `core.ErrorAdaptor.Push` 一条错误；响应状态码 ≥ 400 → 同样推一条错误（含响应体作为 `FullStack`）。
4. 组装 `TracingDetails`（`VerbosityLevel = ThirdParty`）并 `TracingAdaptor.Push`。

出站拦截默认以 `http.DefaultTransport` 为底，适用于所有经 `http.Client` 的第三方调用（如 AMIS/ASIC 等），用于观测外部依赖的调用量、耗时与失败率。

### 错误与定时任务上报流程

*   **错误上报**：业务或拦截器调用 `core.ErrorAdaptor.Push(core.ErrorReport{Error, Uri, FullStack, HappendAT, AppName, AppVersion})`，分发到 Loki / Database / DuckDB / Insights / DataPool。空错误（无文案、无堆栈、无 URI）在 DB 落库时被丢弃。
*   **定时任务**：`gin-shared` 的 schedule 组件在每次 Cron 任务执行完成后，经 `schedule.JobHistoryAdaptor` 分发 `JobHistory`（`Job` / `Cron` / `Duration` / `Succeed` / `Start` / `Finished` 等）。

### 订阅：批量 vs 逐条

`App.go` 的 `SubscribeMonitor`（及导出的 `SubscribeBatch`）是后端订阅的唯一入口，自动感知后端能力：

*   实现 `MonitorBatchService` 的后端（Loki / DuckDB / Database）走**批量订阅** `SubscripterBatch`：每次收到一整批，`BatchSize` 与 `BatchInterval` 按 adaptor 从配置读取，缺省 `200 条 / 30s`（`core.DefaultBatchSize` / `core.DefaultBatchInterval`）。
*   未实现批量接口的后端（Insights / DataPool）回退为**逐条订阅** `Subscripter`，每条消息一个 handler 调用。
*   实现 `Filterable` 的后端（Loki / DuckDB）在批量 handler 内先按 `Included`/`Excluded`/`IncludedIPs`/`ExcludedIPs` 过滤，再决定是否写入。
*   **订阅抖动**：每次批量订阅启动时随机 `sleep 1-5s`（`subscribeJitter`），让相同 `BatchInterval` 的多个消费者错开唤醒/写库时刻，避免同一时间点集中触发大量写入。

### Redis Streams 桥接模式

`messaging/adaptor.go` 的 `EnableRedisAdaptors` 把三个全局 Adaptor 直接替换为 Redis Streams 实现（`monitor.tracing` / `monitor.schedule` / `monitor.error`），生产与消费都基于同一 `core.Adaptor` / `core.BatchingAdaptor` 接口：

```
生产者进程 (monitor_messaging 构建)          消费者进程 (monitor-adaptor / scm-datapool)
───────────────────────────────            ────────────────────────────────────────
Push(TracingDetails) ──► XADD ──► Redis Stream ──► XReadGroup(Count=batchSize, Block=batchInterval)
                                                          │ 整批处理 (各后端 ReportBatch)
                                                          │ 全部成功 → XAck
                                                          └ 任一失败 → 整批保持 pending 待重投
```

要点：

*   无 redis ↔ chan 中间转换层，避免「Redis 已 ack、内存这跳又丢」的丢失窗口。
*   `Block=batchInterval` 是**最长等待**而非固定周期：消息即到即唤醒处理，仅空闲时最多等 `batchInterval`（默认 30s）。
*   消费端写失败（如 Loki push 失败、DuckDB 语句错误、MySQL 插入失败）会让整批保持 pending，由 **pending-check 定时重投**，属 at-least-once 语义（监控数据可接受重复）。
*   **重投与死信**：pending-check 默认 `@every 30m` 运行（`messaging.pendingSchedule` 可配），逐条重投；消息 XADD 后超过 **72 小时**（`DefaultDeadLetterDurtion`，gin-shared `redisStreaming.go` 硬编码常量，非 yaml 可配）仍失败，则 XAck 并写入 `receivedAbandoned.log` 放弃自动重投。
*   当 messaging 后端是 RAM / no_messaging 时保持进程内 chan 不变。

### 批量订阅参数与落库

批量订阅的 `BatchSize` / `BatchInterval` 按 adaptor 从配置读取（`SubscribeBatch` → `batchConfig`），缺省回退 `core.DefaultBatchSize=200` / `core.DefaultBatchInterval=30s`：

| adaptor | 配置 Key |
| :--- | :--- |
| `TracingAdaptor`（`monitor.tracing`） | `tracing.batch.size` / `tracing.batch.interval` |
| `schedule.JobHistoryAdaptor`（`monitor.schedule`） | `schedule.batch.size` / `schedule.batch.interval` |
| `core.ErrorAdaptor`（`monitor.error`） | `error.batch.size` / `error.batch.interval` |

各后端「收到整批 → 真正写库」不再经过进程内攒批缓冲，而是由 handler 直接写入；写失败时（Redis 路径）整批保持 pending 由 pending-check 重投，**保留原有重试机制**：

| 后端 | 落库方式 |
| :--- | :--- |
| Database（`db/todb.go`） | `ReportTracingBatch` / `ReportErrorBatch` 整批直接 `CreateInBatches`（按 `dbInsertChunk=100` 分块），无 `queue` / `lo.BufferWithTimeout` 缓冲 |
| DuckDB（`duckdb/duckdb.go`） | 整批直接拼一条多行 `INSERT` 执行 |
| Loki（`loki/lokimonitor.go`） | handler 同步组装 entries → `pushEntries`（持锁按 `batchMaxBytes` 切块）→ `pushBatch`（指数退避重试）；**成功才返回 nil/ACK**，失败整批 pending 重投，无进程内缓冲 |

*   Database 落库前会过滤：请求体与响应体都为空的记录忽略；`VerbosityLevel > tracing.db.storeMaxVerbosityLevel` 的记录忽略；错误落库需 `tracing.db.error.enabled=true`（默认关闭，避免所有启用 monitor_db 的消费方都写错误表，错误只在 monitor-adaptor 一处统一写入）。
*   错误文案/URI 会 `truncateRunes` 截断（256 / 2048 字符），避免超 varchar 列上限导致整批插入失败；`HappendAT` 零值回退为当前时间。

### Loki 写入与自适应限速流程

Loki 后端已把 **ACK 后移到「真正 push 到 Loki 成功」之后**：每个批量 handler 同步推送，成功才返回 nil（Redis 侧才 ACK），不再有进程内异步缓冲队列，进程崩溃不会丢「已入队未推送」的数据：

1. `ReportTracingBatch` / `ReportErrorBatch` / `ReportScheduleJobBatch` 组装 label + 正文，超长正文按 `MaxBytes`（240KiB）拆分（`[part n/N]` 前缀，不把分片序号写入 label 避免高基数）。
2. `pushEntries` 持 `sync.Mutex` 串行化推送，按 `batchMaxBytes`（1MB）切块（tracing/schedule/error 三个批量消费者并发时互斥）。
3. `pushBatch` 遇到 429/超时/5xx 等可重试错误按指数退避重试（最多 `maxPushAttempts=6` 次）；不可重试或重试耗尽则返回 error → 整批保持 pending 由 pending-check 重投。
4. **自适应限速**：发生重试则写间隔倍增（上限 `maxRetryPause`=15s），干净成功则减半（下限 `minPause`=20ms），启动后 `startupSlowStartSeconds`（120s）窗口内用更大的 `startupPause`（250ms）平滑突发。
5. 停机：`close` 置 `closed` 标志并关客户端，重试中的推送随即停止并返回 error（pending 重投）。

### DuckDB 写入流程

*   惰性连接：首个批量到达时才 `Dial`，并幂等建表（`CREATE TABLE IF NOT EXISTS`）。
*   每次批量把整批拼成一条多行 `INSERT`，经纯 Go 的 Quack 客户端（`duckcall`，无 CGO）提交到远端 DuckDB 实例。
*   `exec` 持 `sync.Mutex` 串行化写入；仅 `ErrConnectionExpired`（会话被服务器遗忘）触发一次重连，语句错误直接透出，让 Redis 保持整批 pending。
*   表名按 `tablePrefix` 前缀隔离（`prd_` / `uat_`），对应 `monitor_tracing` / `monitor_error` / `monitor_job_history` 三张表。

### 数据清理流程（分级保留）

`cleanup/cleanup.go` 是 MySQL 与 DuckDB 共用的分级清理模块，默认策略 `DefaultTracingPolicy`：

| verbosity_level | 保留时长 |
| :--- | :--- |
| `≤ 10`（MostImportant / ThirdParty） | 6 个月 |
| `(10, 50]`（Write） | 14 天 |
| `> 50`（Read） | 3 天 |

*   调度默认 cron `11 2 * * 0`（每周日 02:11），可通过 `tracing.db.cleanup.schedule` / `tracing.duckdb.cleanup.schedule` 覆盖，`""` / `"-"` 禁用。
*   MySQL 清理 `created_at <= cutoff` 的记录，DuckDB 清理 `started_at <= cutoff` 的记录，两者按 `verbosity_level` 分档删除（`(minExclusive, max]`）。

## 主要组件 (Key Components)

### 数据采集 (Instrumentation)
*   **Gin 中间件 (`gin.go`)**: 自动拦截 HTTP 请求，记录请求体、响应体、耗时、状态码、Client IP、User Agent 等信息，并封装为 `TracingDetails` 对象。
*   **HTTP 客户端拦截 (`requesttracing.go`)**: 提供了 `http.RoundTripper` 的包装器，用于拦截和记录该应用发出的对外 HTTP 请求（Outbound Traffic）。
*   **MQTT 订阅 (`mqtt/`)**: 订阅 MQTT Topic，将接收到的消息转换为 `TracingDetails` 进行处理。

### 后端实现 (Backends)
项目提供了多种开箱即用的监控后端实现：
*   **Loki (`loki/`)**: 将日志和追踪数据推送到 Grafana Loki。支持 gRPC 协议，性能更高。
*   **Azure Application Insights (`insights/`)**: 集成 Azure 的 APM 服务。
*   **Database (`db/`)**: 使用 GORM 将监控数据持久化到关系型数据库（如 MySQL, PostgreSQL）。
*   **DuckDB (`duckdb/`)**: 经 Quack 远程协议把 tracing/error/schedule 批量写入 DuckDB 实例。
*   **DataPool (`datapool/`)**: 将数据保存为 Parquet 文件，通常用于大数据分析或归档。
*   **Console**: 直接输出到控制台，便于开发调试。

#### Loki 配置 (Protocol 选择)
通过 `tracing.loki` 配置可以选择客户端协议，默认使用 REST，支持 BasicAuth。

```yaml
tracing:
  loki:
    URL: http://localhost:3100
    User: admin
    Password: secret
    Protocol: rest  # 可选: "rest" | "grpc"，缺省为 "rest"
    MaxBytes: 245760 # 可选：单条日志行最大字节数（默认 240KiB，避免贴近 Loki 256KiB 上限）
```

行为说明：
- Protocol = "rest" 或缺省：优先 REST，REST 初始化失败时自动回退到 gRPC
- Protocol = "grpc"：优先 gRPC，gRPC 初始化失败时自动回退到 REST

#### Loki 行大小与二进制内容处理
Tracing/Error/Job 推送到 Loki 时，会尽量保证内容可被 Loki 接受：

- 单条日志行过大：会按 `MaxBytes` 自动拆分为多条日志（同一组 labels，不将分片序号写入 label，避免高基数）
- Body/Resp/Stack 可能为纯二进制：会进行文本化编码（并通过 `reqEnc` / `respEnc` / `stackEnc` label 标识编码方式）
- REST 模式出错：会把 Loki 返回的 HTTP 状态码与响应体带回到错误信息中，便于定位 400/鉴权/限额等原因

### 启动与集成 (`bootup/`)
包含各个模块的初始化代码，利用依赖注入机制来自动装配启用的监控服务。
这些初始化代码通过 Build Tags 控制，确保只有被选中的模块才会被编译和注册。

## 数据模型

*   **TracingDetails (`tracing.go`)**: 非常详尽的请求记录结构，不仅包含标准的 HTTP 信息，还包含多租户信息（Tenant, Operator）和设备信息，说明这个监控系统是为多租户 SaaS 应用设计的。

### Database 数据模型 (monitor_db)
启用 `monitor_db` 后，会自动注册 GORM 实体并订阅监控事件入库：

- Tracing：`FullRequestDetails`
- Cron Job：`JobHistoryDetails`
- Error：`ErrorReportDetails`（其中 `error` 以 `ErrorText` 字符串持久化）

## 项目亮点

*   **非侵入式**: 通过中间件和全局配置即可启用，对业务逻辑代码侵入极小。
*   **扩展性强**: 如果需要支持新的监控系统（比如 Elasticsearch），只需实现 `MonitorService` 接口并注册即可。
*   **多维度**: 不仅仅是日志（Logs），还涵盖了追踪（Tracing）、错误（Errors）和任务监控（Jobs）。
