# monitor/db

## DB 清理任务

该清理任务用于清理由 `monitor/db` 写入的请求追踪明细表数据。

### 配置 Key

- `tracing.db`
- `tracing.db.cleanup`

### 启用方式

默认会注册定时任务，执行时间为每周日 02:11。

如需覆盖或禁用：
- 覆盖：设置 `tracing.db.cleanup.schedule`
- 禁用：设置 `tracing.db.cleanup.schedule: "-"`（框架约定 `""` / `"-"` 为禁用）

### 默认行为

- 分段保留策略（按 `verbosity_level`）：
  - `0-10`：保留 6 个月
  - `(10, 50]`：保留 14 天
  - `(50, +)`：保留 3 天

### 配置示例

```yaml
tracing:
  db:
    storeMaxVerbosityLevel: 50
    cleanup:
      schedule: "11 2 * * 0"
```

### 字段说明

- `schedule`：cron 表达式；为空则不注册
