// Package duckdb 提供 monitor 数据的 DuckDB 落库消费者（Quack 远程协议 + 批量模式）。
//
// 通过 Redis Streams 的批量订阅（gin-shared BatchingAdaptor：200 条 / 1s 整批、
// 整批成功才 XAck）接收 tracing / error / schedule / connection 四类监控数据，每个
// 批量方法把整批拼成一条多行 INSERT，经本仓库的 quack 客户端（底层官方
// github.com/duckdb/duckdb-go/v2，内嵌 DuckDB 2.x，CGO）提交到 DuckDB 实例。
//
// 【2026-10-26 客户端切换】原先用纯 Go 的 github.com/mehrabr/duckcall。但 Quack
// wire 协议按大版本锁定（1.5.5 ↔ 2.0 双向不兼容），而 duckcall 上游最新版仍只实现
// wire v1，没有 2.0 版本可升。故改用官方驱动；统一经 r.query() 透传，语义等价。
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
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"github.com/techquest-tech/monitor/quack"
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
	conn *quack.Conn
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
			// 启动时就把连接与表结构（含新增的 blob 引用列）准备好，
			// 避免读侧（MCP 查询）在低流量时先于首次写入而撞上缺列。
			if err := s.EnsureSchema(context.Background()); err != nil {
				logger.Warn("duckdb ensure schema at startup failed, will retry lazily", zap.Error(err))
			}
			monitor.SubscribeMonitor(logger, s)
			if err := s.scheduleCleanup(logger); err != nil {
				logger.Error("schedule duckdb cleanup job failed", zap.Error(err))
			}
		}
		return nil
	})
}

// EnsureSchema 主动建连并幂等建表/补列（启动时调用；失败仅告警，写入路径仍会惰性重试）。
func (s *DuckDBBatchService) EnsureSchema(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return nil
	}
	return s.dialLocked(ctx)
}

// Close 关闭底层 quack 连接（幂等，未连接时无操作）。
func (s *DuckDBBatchService) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
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

// ReportConnectionBatch writes connection-level events in one INSERT.
// It implements monitor.ConnectionService.
func (s *DuckDBBatchService) ReportConnectionBatch(cds []monitor.ConnectionDetails) error {
	if len(cds) == 0 {
		return nil
	}
	return s.execWithTimeout(buildConnectionInsert(s.config.TablePrefix, cds))
}

// --- 连接与执行 ---

func (s *DuckDBBatchService) execWithTimeout(sql string) error {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(s.config.WriteTimeoutMS)*time.Millisecond)
	defer cancel()
	return s.exec(ctx, sql)
}

// exec 在远端执行一条 SQL（建表/写入均走此路径）。
// 连接失效（服务端重启/会话回收）由 quack.Conn 内部重连并重试一次；
// 语句本身错误原样透出，让 Redis 保持整批 pending 待重投。
func (s *DuckDBBatchService) exec(ctx context.Context, sql string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		if err := s.dialLocked(ctx); err != nil {
			return err
		}
	}
	return s.conn.Exec(ctx, sql)
}

// execRows 执行一条会返回受影响行数的语句（DELETE 等），返回首行首列的整数。
func (s *DuckDBBatchService) execRows(ctx context.Context, sql string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		if err := s.dialLocked(ctx); err != nil {
			return 0, err
		}
	}
	rows, err := s.conn.Query(ctx, sql)
	if err != nil {
		return 0, err
	}
	if len(rows) > 0 && len(rows[0]) > 0 {
		return countValue(rows[0][0]), nil
	}
	return 0, nil
}

// queryValue 执行单值查询并返回第一行第一列（nil = 无结果/NULL），供观测/测试。
func (s *DuckDBBatchService) queryValue(ctx context.Context, sql string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		if err := s.dialLocked(ctx); err != nil {
			return nil, err
		}
	}
	rows, err := s.conn.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 && len(rows[0]) > 0 {
		return rows[0][0], nil
	}
	return nil, nil
}

// dialLocked 建立 quack 连接并幂等建表（CREATE TABLE IF NOT EXISTS，重连后无害重跑）。
func (s *DuckDBBatchService) dialLocked(ctx context.Context) error {
	conn, err := quack.Dial(ctx, quack.Config{
		Endpoint: s.config.Endpoint,
		Token:    s.config.Token,
	})
	if err != nil {
		return fmt.Errorf("duckdb dial %s: %w", s.config.Endpoint, err)
	}
	s.conn = conn
	for _, ddl := range s.ddlStatements() {
		if err := conn.Exec(ctx, ddl); err != nil {
			_ = conn.Close()
			s.conn = nil
			return fmt.Errorf("duckdb ensure schema: %w", err)
		}
	}
	// 老表补列：CREATE TABLE IF NOT EXISTS 不会给既有表加列。
	if err := s.ensureTracingBlobColumns(ctx); err != nil {
		_ = conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

// tracingBlobColumns 是 monitor_tracing 的「大字段外置」引用列（2026-10-12 新增）：
// 单条 body/resp 实测可达 24 MB，内容改为落存储后端后，这里只存引用与元信息。
// 列名与顺序必须与 tracingColumns / buildTracingInsert 保持一致。
var tracingBlobColumns = []struct{ name, ddl string }{
	{"body_blob_ref", "VARCHAR"},    // "key:path"（storage 配置键 + 相对路径）
	{"body_blob_size", "BIGINT"},    // 原始字节数
	{"body_blob_sha256", "VARCHAR"}, // 内容摘要
	{"resp_blob_ref", "VARCHAR"},
	{"resp_blob_size", "BIGINT"},
	{"resp_blob_sha256", "VARCHAR"},
}

// ensureTracingBlobColumns 幂等补齐 monitor_tracing 的外置引用列。
// 在持有 s.mu 的前提下由 dialLocked 调用，因此这里直接用 s.conn，不能再走 exec/queryValue（会自锁）。
func (s *DuckDBBatchService) ensureTracingBlobColumns(ctx context.Context) error {
	table := s.config.TablePrefix + "monitor_tracing"
	for _, col := range tracingBlobColumns {
		sql := "SELECT count(*) FROM information_schema.columns WHERE table_name = " +
			sqlString(table) + " AND column_name = " + sqlString(col.name)
		rows, err := s.conn.Query(ctx, sql)
		if err != nil {
			return fmt.Errorf("duckdb ensure column %s: %w", col.name, err)
		}
		var exists bool
		if len(rows) > 0 && len(rows[0]) > 0 {
			exists = countValue(rows[0][0]) > 0
		}
		if exists {
			continue
		}
		alter := "ALTER TABLE " + table + " ADD COLUMN " + col.name + " " + col.ddl
		if err := s.conn.Exec(ctx, alter); err != nil {
			return fmt.Errorf("duckdb add column %s: %w", col.name, err)
		}
		zap.L().Info("duckdb added tracing blob column",
			zap.String("table", table), zap.String("column", col.name))
	}
	return nil
}

// countValue 把 count(*) 的结果（可能是 int32/int64/其他数值类型）统一成 int64。
func countValue(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case uint64:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// ddlStatements 返回四张监控表的幂等建表语句（表名带 tablePrefix）。
func (s *DuckDBBatchService) ddlStatements() []string {
	p := s.config.TablePrefix
	return []string{
		"CREATE TABLE IF NOT EXISTS " + p + "monitor_tracing (" +
			"started_at TIMESTAMPTZ, optionname VARCHAR, uri VARCHAR, method VARCHAR, " +
			"app_name VARCHAR, app_version VARCHAR, verbosity_level INTEGER, " +
			"body BLOB, body_enc VARCHAR, duration_ms BIGINT, status INTEGER, target_id BIGINT, " +
			"resp BLOB, resp_enc VARCHAR, client_ip VARCHAR, user_agent VARCHAR, " +
			"device VARCHAR, tenant VARCHAR, operator VARCHAR, " +
			"body_blob_ref VARCHAR, body_blob_size BIGINT, body_blob_sha256 VARCHAR, " +
			"resp_blob_ref VARCHAR, resp_blob_size BIGINT, resp_blob_sha256 VARCHAR)",
		"CREATE TABLE IF NOT EXISTS " + p + "monitor_error (" +
			"happened_at TIMESTAMPTZ, app_name VARCHAR, app_version VARCHAR, uri VARCHAR, " +
			"error_text VARCHAR, full_stack BLOB)",
		"CREATE TABLE IF NOT EXISTS " + p + "monitor_job_history (" +
			"start_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, next_at TIMESTAMPTZ, " +
			"app VARCHAR, app_version VARCHAR, job VARCHAR, cron VARCHAR, " +
			"duration_ms BIGINT, succeed BOOLEAN, message VARCHAR, disabled BOOLEAN)",
		"CREATE TABLE IF NOT EXISTS " + p + "monitor_connection (" +
			"happened_at TIMESTAMPTZ, action VARCHAR, conn_id VARCHAR, started_at TIMESTAMPTZ, " +
			"duration_ms BIGINT, rpcs BIGINT, peer VARCHAR, local_addr VARCHAR, client_ip VARCHAR, " +
			"client VARCHAR, device VARCHAR, sn VARCHAR, app_name VARCHAR, app_version VARCHAR, " +
			"tenant VARCHAR, reason VARCHAR)",
	}
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
	"body, body_enc, duration_ms, status, target_id, resp, resp_enc, client_ip, user_agent, device, tenant, operator, " +
	"body_blob_ref, body_blob_size, body_blob_sha256, resp_blob_ref, resp_blob_size, resp_blob_sha256)"

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
		fmt.Fprintf(&b, "(%s, %s, %s, %s, %s, %s, %d, %s, %s, %d, %d, %d, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)",
			tsLiteral(tr.StartedAt), sqlString(tr.Optionname), sqlString(tr.Uri), sqlString(tr.Method),
			sqlString(tr.AppName), sqlString(tr.AppVersion), int(tr.VerbosityLevel),
			blobLiteral(tr.Body), sqlString(tr.BodyEnc), durationMs(tr.Durtion),
			tr.Status, int64(tr.TargetID), blobLiteral(tr.Resp), sqlString(tr.RespEnc),
			sqlString(tr.ClientIP), sqlString(tr.UserAgent), sqlString(tr.Device),
			sqlString(tr.Tenant), sqlString(tr.Operator),
			blobRefLiteral(tr.BodyBlob), blobSizeLiteral(tr.BodyBlob), blobSHA256Literal(tr.BodyBlob),
			blobRefLiteral(tr.RespBlob), blobSizeLiteral(tr.RespBlob), blobSHA256Literal(tr.RespBlob))
	}
	return b.String()
}

// connectionColumns must stay in sync with the monitor_connection DDL and with
// buildConnectionInsert.
const connectionColumns = "(happened_at, action, conn_id, started_at, duration_ms, rpcs, " +
	"peer, local_addr, client_ip, client, device, sn, app_name, app_version, tenant, reason)"

// buildConnectionInsert builds one batched INSERT for connection events.
func buildConnectionInsert(prefix string, cds []monitor.ConnectionDetails) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(prefix)
	b.WriteString("monitor_connection ")
	b.WriteString(connectionColumns)
	b.WriteString(" VALUES ")
	for i, cd := range cds {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%s, %s, %s, %s, %d, %d, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)",
			tsLiteral(cd.HappenedAt), sqlString(string(cd.Action)), sqlString(cd.ConnID),
			tsLiteral(cd.StartedAt), durationMs(cd.Duration), cd.RPCs,
			sqlString(cd.Peer), sqlString(cd.LocalAddr), sqlString(cd.ClientIP),
			sqlString(cd.Client), sqlString(cd.Device), sqlString(cd.SN),
			sqlString(cd.AppName), sqlString(cd.AppVersion), sqlString(cd.Tenant),
			sqlString(cd.Reason))
	}
	return b.String()
}

// blobRefLiteral 把 BlobRef 序列化为 "key:path"（storage 配置键 + 相对路径）；
// nil 或路径为空时写 NULL。
func blobRefLiteral(ref *monitor.BlobRef) string {
	if ref == nil || ref.Path == "" {
		return "NULL"
	}
	return sqlString(ref.Key + ":" + ref.Path)
}

// blobSizeLiteral 返回引用对象的原始字节数（无引用时 NULL）。
func blobSizeLiteral(ref *monitor.BlobRef) string {
	if ref == nil || ref.Path == "" {
		return "NULL"
	}
	return strconv.FormatInt(ref.Size, 10)
}

// blobSHA256Literal 返回引用对象的内容摘要（无引用时 NULL）。
func blobSHA256Literal(ref *monitor.BlobRef) string {
	if ref == nil || ref.Path == "" || ref.SHA256 == "" {
		return "NULL"
	}
	return sqlString(ref.SHA256)
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
