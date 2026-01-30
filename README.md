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
*   **DataPool (`datapool/`)**: 将数据保存为 Parquet 文件，通常用于大数据分析或归档。
*   **Console**: 直接输出到控制台，便于开发调试。

### 启动与集成 (`bootup/`)
包含各个模块的初始化代码，利用依赖注入机制来自动装配启用的监控服务。
这些初始化代码通过 Build Tags 控制，确保只有被选中的模块才会被编译和注册。

## 数据模型

*   **TracingDetails (`tracing.go`)**: 非常详尽的请求记录结构，不仅包含标准的 HTTP 信息，还包含多租户信息（Tenant, Operator）和设备信息，说明这个监控系统是为多租户 SaaS 应用设计的。

## 项目亮点

*   **非侵入式**: 通过中间件和全局配置即可启用，对业务逻辑代码侵入极小。
*   **扩展性强**: 如果需要支持新的监控系统（比如 Elasticsearch），只需实现 `MonitorService` 接口并注册即可。
*   **多维度**: 不仅仅是日志（Logs），还涵盖了追踪（Tracing）、错误（Errors）和任务监控（Jobs）。
