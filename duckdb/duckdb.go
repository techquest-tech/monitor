// Package duckdb 提供 monitor 数据的 DuckDB 落库消费者（Quack 远程协议 + 批量模式）。
//
// 通过 Redis Streams 的批量订阅（gin-shared BatchingAdaptor：200 条 / 1s 整批、
// 整批成功才 XAck）接收 tracing / error / schedule 三类监控数据，每个批量方法把
// 整批拼成一条多行 INSERT，经纯 Go 的 Quack 客户端 github.com/mehrabr/duckcall
// （无 CGO、无内嵌引擎）直接提交到 176.87 的 DuckDB 实例。
//
// 说明：duckcall 的 database/sql 驱动是只读的（Exec 返回 ErrReadOnly），但其
// native API Conn.Query 可执行任意 SQL（DDL/DML 均实测通过，服务器返回受影响
// 行数作为单行结果），本包直接使用 native API 完成批量写入。
//
// 用法：monitor-adaptor 的 Makefile build tags 增加 monitor_duckdb（bootup/duckdb.go
// 自动注册），配置 tracing.duckdb.*：
//
//	tracing:
//	  duckdb:
//	    enabled: true
//	    endpoint: localhost:9494      # 与 duckdb-quack 容器同机，走回环零网络依赖
//	    token: <64hex>                # 或用环境变量 TRACING_DUCKDB_TOKEN 覆盖
//	    tablePrefix: prd_             # prd / uat 同实例分离
package duckdb

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mehrabr/duckcall"
	"github.com/mehrabr/duckcall/wire"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

// Config 是 tracing.duckdb 的配置项（viper 键名与 yaml 字段一一对应）。
type Config struct {
	Enabled        bool     `mapstructure:"enabled"`
	Endpoint       string   `mapstructure:"endpoint"`
	Token          string   `mapstructure:"token"`
	TablePrefix    string   `mapstructure:"tablePrefix"`
	WriteTimeoutMS int      `mapstructure:"writeTimeoutMS"`
	Included       []string `mapstructure:"included"`
	Excluded       []string `mapstructure:"excluded"`
	IncludedIPs    []string `mapstructure:"includedIPs"`
	ExcludedIPs    []string `mapstructure:"excludedIPs"`
}

// DuckDBBatchService 实现 monitor.MonitorBatchService（批量）+ MonitorService（单条，
// 委托批量），由 monitor.SubscribeMonitor 自动走 SubscripterBatch 批量订阅路径。
type DuckDBBatchService struct {
	monitor.BaseFilter

	config *Config
	logger *zap.Logger

	mu   sync.Mutex // 串行化写入 + 保护 conn 重连
	conn *duckcall.Conn
}

// loadConfig 从 viper 读取 tracing.duckdb，应用默认值。
func loadConfig(logger *zap.Logger) *Config {
	cfg := &Config{}
	if err := viper.UnmarshalKey("tracing.duckdb", cfg); err != nil {
		logger.Warn("duckdb config unmarshal failed, use defaults", zap.Error(err))
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "localhost:9494"
	}
	// tablePrefix 默认空 → 表名 monitor_tracing / monitor_error / monitor_job_history；
	// prd/uat 同实例部署时各自配置 prd_ / uat_ 前缀做隔离（对应 MySQL prd_* 表约定）。
	if cfg.WriteTimeoutMS <= 0 {
		cfg.WriteTimeoutMS = 30000
	}
	return cfg
}

// NewDuckDBBatchService 创建批量落库服务。连接惰性建立：首个批量到达时才 Dial。
func NewDuckDBBatchService(cfg *Config, logger *zap.Logger) *DuckDBBatchService {
	s := &DuckDBBatchService{
		config: cfg,
		logger: logger,
	}
	s.BaseFilter = monitor.BaseFilter{
		Included:    cfg.Included,
		Excluded:    cfg.Excluded,
		IncludedIPs: cfg.IncludedIPs,
		ExcludedIPs: cfg.ExcludedIPs,
	}
	return s
}

// InitDuckDBMonitor 是 DI provider：未启用或缺少 token 时返回 nil（不注册消费者）。
func InitDuckDBMonitor(logger *zap.Logger) (*DuckDBBatchService, error) {
	cfg := loadConfig(logger)
	if !cfg.Enabled {
		logger.Info("duckdb monitor disabled (tracing.duckdb.enabled=false)")
		return nil, nil
	}
	if cfg.Token == "" {
		logger.Warn("duckdb monitor: tracing.duckdb.token is empty, consumer disabled")
		return nil, nil
	}
	logger.Info("duckdb monitor enabled",
		zap.String("endpoint", cfg.Endpoint),
		zap.String("tablePrefix", cfg.TablePrefix),
		zap.Duration("writeTimeout", time.Duration(cfg.WriteTimeoutMS)*time.Millisecond),
		zap.Strings("included", cfg.Included),
		zap.Strings("excluded", cfg.Excluded),
	)
	return NewDuckDBBatchService(cfg, logger), nil
}

// EnableDuckDBMonitor 注册 duckdb 消费者（bootup/duckdb.go 在 build tag
// monitor_duckdb 下调用）。必须在 ginshared.Start()（消费者订阅）之前生效，
// 与 loki/db 消费者相同的注册时机。启用后同时注册 DuckDB 分级清理任务
// （monitor_duckdb_cleanup，与 monitor_db_cleanup 同一套共用策略）。
func EnableDuckDBMonitor() {
	core.Provide(InitDuckDBMonitor)
	core.ProvideStartup(func(logger *zap.Logger, s *DuckDBBatchService) core.Startup {
		if s != nil {
			monitor.SubscribeMonitor(logger, s)
			if err := s.scheduleCleanup(logger); err != nil {
				logger.Error("schedule duckdb cleanup job failed", zap.Error(err))
			}
		}
		return nil
	})
}

// Close 关闭底层 quack 连接（幂等，未连接时无操作）。
func (s *DuckDBBatchService) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close(ctx)
	s.conn = nil
	return err
}

// --- MonitorService 单条方法（批量模式下基本不被调用，委托批量实现） ---

func (s *DuckDBBatchService) ReportTracing(tr monitor.TracingDetails) error {
	return s.ReportTracingBatch([]monitor.TracingDetails{tr})
}

func (s *DuckDBBatchService) ReportError(er core.ErrorReport) error {
	return s.ReportErrorBatch([]core.ErrorReport{er})
}

func (s *DuckDBBatchService) ReportScheduleJob(req schedule.JobHistory) error {
	return s.ReportScheduleJobBatch([]schedule.JobHistory{req})
}

// --- MonitorBatchService 批量方法：整批一条 INSERT ---

// ReportTracingBatch 批量写入请求追踪详情。
func (s *DuckDBBatchService) ReportTracingBatch(trs []monitor.TracingDetails) error {
	if len(trs) == 0 {
		return nil
	}
	sql := buildTracingInsert(s.config.TablePrefix, trs)
	return s.execWithTimeout(sql)
}

// ReportErrorBatch 批量写入错误上报。
func (s *DuckDBBatchService) ReportErrorBatch(rrs []core.ErrorReport) error {
	if len(rrs) == 0 {
		return nil
	}
	sql := buildErrorInsert(s.config.TablePrefix, rrs)
	return s.execWithTimeout(sql)
}

// ReportScheduleJobBatch 批量写入定时任务历史。
func (s *DuckDBBatchService) ReportScheduleJobBatch(reqs []schedule.JobHistory) error {
	if len(reqs) == 0 {
		return nil
	}
	sql := buildJobHistoryInsert(s.config.TablePrefix, reqs)
	return s.execWithTimeout(sql)
}

// --- 连接与执行 ---

func (s *DuckDBBatchService) execWithTimeout(sql string) error {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(s.config.WriteTimeoutMS)*time.Millisecond)
	defer cancel()
	res, err := s.exec(ctx, sql)
	if err != nil {
		return err
	}
	return drain(ctx, res)
}

// exec 在远端执行一条 SQL（建表/写入均走此路径），连接失效时自动重连一次。
func (s *DuckDBBatchService) exec(ctx context.Context, sql string) (*duckcall.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queryLocked(ctx, sql, 1)
}

// queryValue 执行单值查询并返回第一行第一列（nil = 无结果/NULL），供观测/测试。
func (s *DuckDBBatchService) queryValue(ctx context.Context, sql string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.queryLocked(ctx, sql, 1)
	if err != nil {
		return nil, err
	}
	for ch, err := range res.Chunks(ctx) {
		if err != nil {
			return nil, err
		}
		if ch.RowCount() > 0 && ch.ColumnCount() > 0 {
			return ch.Value(0, 0)
		}
	}
	return nil, nil
}

// queryLocked 在持有 mu 的前提下执行；首次调用（或重连后）自动 Dial 并幂等建表。
func (s *DuckDBBatchService) queryLocked(ctx context.Context, sql string, retries int) (*duckcall.Result, error) {
	if s.conn == nil {
		if err := s.dialLocked(ctx); err != nil {
			return nil, err
		}
	}
	res, err := s.conn.Query(ctx, sql)
	if err != nil {
		// 仅连接过期（会话被服务器遗忘）触发重连；语句错误（如表不存在）直接透出，
		// 让 Redis 保持整批 pending 待重投。
		if retries > 0 && errors.Is(err, wire.ErrConnectionExpired) {
			s.conn = nil
			return s.queryLocked(ctx, sql, retries-1)
		}
		return nil, err
	}
	return res, nil
}

// dialLocked 建立 quack 连接并幂等建表（CREATE TABLE IF NOT EXISTS，重连后无害重跑）。
func (s *DuckDBBatchService) dialLocked(ctx context.Context) error {
	conn, err := duckcall.Dial(ctx, duckcall.Config{
		Endpoint: "http://" + s.config.Endpoint,
		Token:    s.config.Token,
	})
	if err != nil {
		return fmt.Errorf("duckdb dial %s: %w", s.config.Endpoint, err)
	}
	s.conn = conn
	for _, ddl := range s.ddlStatements() {
		res, err := s.conn.Query(ctx, ddl)
		if err != nil {
			return fmt.Errorf("duckdb ensure schema: %w", err)
		}
		if err := drain(ctx, res); err != nil {
			return fmt.Errorf("duckdb ensure schema: %w", err)
		}
	}
	return nil
}

// ddlStatements 返回三张监控表的幂等建表语句（表名带 tablePrefix）。
func (s *DuckDBBatchService) ddlStatements() []string {
	p := s.config.TablePrefix
	return []string{
		"CREATE TABLE IF NOT EXISTS " + p + "monitor_tracing (" +
			"started_at TIMESTAMPTZ, optionname VARCHAR, uri VARCHAR, method VARCHAR, " +
			"app_name VARCHAR, app_version VARCHAR, verbosity_level INTEGER, " +
			"body BLOB, body_enc VARCHAR, duration_ms BIGINT, status INTEGER, target_id BIGINT, " +
			"resp BLOB, resp_enc VARCHAR, client_ip VARCHAR, user_agent VARCHAR, " +
			"device VARCHAR, tenant VARCHAR, operator VARCHAR)",
		"CREATE TABLE IF NOT EXISTS " + p + "monitor_error (" +
			"happened_at TIMESTAMPTZ, app_name VARCHAR, app_version VARCHAR, uri VARCHAR, " +
			"error_text VARCHAR, full_stack BLOB)",
		"CREATE TABLE IF NOT EXISTS " + p + "monitor_job_history (" +
			"start_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, next_at TIMESTAMPTZ, " +
			"app VARCHAR, app_version VARCHAR, job VARCHAR, cron VARCHAR, " +
			"duration_ms BIGINT, succeed BOOLEAN, message VARCHAR, disabled BOOLEAN)",
	}
}

// drain 消费完整个结果集（INSERT 等无结果语句的服务器侧执行已在 Query 时完成，
// 此处兜底捕获流式阶段的错误并释放服务器端结果状态）。
func drain(ctx context.Context, res *duckcall.Result) error {
	defer res.Close(ctx)
	for ch, err := range res.Chunks(ctx) {
		if err != nil {
			return err
		}
		_ = ch
	}
	return nil
}

// --- SQL 构建（可单测的纯函数） ---

// sqlString 生成 DuckDB 标准字符串字面量（单引号翻倍转义）。
func sqlString(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// blobLiteral 生成 BLOB 字面量；空字节片返回 NULL。
func blobLiteral(b []byte) string {
	if len(b) == 0 {
		return "NULL"
	}
	return "from_hex('" + hex.EncodeToString(b) + "')"
}

// tsLiteral 生成 TIMESTAMPTZ 字面量（UTC）；零值返回 NULL。
func tsLiteral(t time.Time) string {
	if t.IsZero() {
		return "NULL"
	}
	return "'" + t.UTC().Format("2006-01-02 15:04:05.999999Z07:00") + "'::TIMESTAMPTZ"
}

// durationMs 把 time.Duration 转毫秒整数。
func durationMs(d time.Duration) int64 {
	return int64(d / time.Millisecond)
}

const tracingColumns = "(started_at, optionname, uri, method, app_name, app_version, verbosity_level, " +
	"body, body_enc, duration_ms, status, target_id, resp, resp_enc, client_ip, user_agent, device, tenant, operator)"

// buildTracingInsert 构造批量 INSERT（含表名前缀）。
func buildTracingInsert(prefix string, trs []monitor.TracingDetails) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(prefix)
	b.WriteString("monitor_tracing ")
	b.WriteString(tracingColumns)
	b.WriteString(" VALUES ")
	for i, tr := range trs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%s, %s, %s, %s, %s, %s, %d, %s, %s, %d, %d, %d, %s, %s, %s, %s, %s, %s, %s)",
			tsLiteral(tr.StartedAt), sqlString(tr.Optionname), sqlString(tr.Uri), sqlString(tr.Method),
			sqlString(tr.AppName), sqlString(tr.AppVersion), int(tr.VerbosityLevel),
			blobLiteral(tr.Body), sqlString(tr.BodyEnc), durationMs(tr.Durtion),
			tr.Status, int64(tr.TargetID), blobLiteral(tr.Resp), sqlString(tr.RespEnc),
			sqlString(tr.ClientIP), sqlString(tr.UserAgent), sqlString(tr.Device),
			sqlString(tr.Tenant), sqlString(tr.Operator))
	}
	return b.String()
}

const errorColumns = "(happened_at, app_name, app_version, uri, error_text, full_stack)"

// buildErrorInsert 构造错误上报批量 INSERT。
func buildErrorInsert(prefix string, rrs []core.ErrorReport) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(prefix)
	b.WriteString("monitor_error ")
	b.WriteString(errorColumns)
	b.WriteString(" VALUES ")
	for i, er := range rrs {
		if i > 0 {
			b.WriteString(", ")
		}
		text := ""
		if er.Error != nil {
			text = er.Error.Error()
		}
		fmt.Fprintf(&b, "(%s, %s, %s, %s, %s, %s)",
			tsLiteral(er.HappendAT), sqlString(er.AppName), sqlString(er.AppVersion),
			sqlString(er.Uri), sqlString(text), blobLiteral(er.FullStack))
	}
	return b.String()
}

const jobColumns = "(start_at, finished_at, next_at, app, app_version, job, cron, " +
	"duration_ms, succeed, message, disabled)"

// buildJobHistoryInsert 构造定时任务历史批量 INSERT。
func buildJobHistoryInsert(prefix string, reqs []schedule.JobHistory) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(prefix)
	b.WriteString("monitor_job_history ")
	b.WriteString(jobColumns)
	b.WriteString(" VALUES ")
	for i, j := range reqs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%s, %s, %s, %s, %s, %s, %s, %d, %s, %s, %s)",
			tsLiteral(j.Start), tsLiteral(j.Finished), tsLiteral(j.Next),
			sqlString(j.App), sqlString(j.AppVersion), sqlString(j.Job), sqlString(j.Cron),
			durationMs(j.Duration), boolLiteral(j.Succeed), sqlString(j.Message), boolLiteral(j.Disabled))
	}
	return b.String()
}

// boolLiteral 生成 DuckDB 布尔字面量。
func boolLiteral(v bool) string {
	if v {
		return "TRUE"
	}
	return "FALSE"
}
