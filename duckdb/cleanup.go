package duckdb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/techquest-tech/monitor/cleanup"
	"go.uber.org/zap"
)

// tracingCleaner 实现 cleanup.Cleaner：按 verbosity 分档删除 DuckDB 的
// <prefix>monitor_tracing（时间列 started_at，业务发生时间）。
type tracingCleaner struct {
	svc *DuckDBBatchService
}

// CleanTier 删除 started_at <= cutoff 且 verbosity_level ∈ (minExclusive, max] 的记录。
// DELETE 经 duckcall 远端执行，返回受影响行数（服务器结果首行）。
func (c *tracingCleaner) CleanTier(ctx context.Context, minExclusive, max int, cutoff time.Time) (int64, error) {
	sql := buildCleanTierDeleteSQL(c.svc.config.TablePrefix, minExclusive, max, cutoff)
	res, err := c.svc.exec(ctx, sql)
	if err != nil {
		return 0, err
	}
	defer res.Close(ctx)
	// DELETE 结果为首行单列（受影响行数），解析失败仅影响统计日志，不阻断清理。
	n := int64(0)
	for ch, err := range res.Chunks(ctx) {
		if err != nil {
			return n, err
		}
		if ch.RowCount() > 0 && ch.ColumnCount() > 0 {
			if v, err := ch.Value(0, 0); err == nil {
				if iv, ok := v.(int64); ok {
					n = iv
				}
			}
		}
	}
	return n, nil
}

// buildCleanTierDeleteSQL 构造分级删除 SQL（纯函数，可单测）。
// 区间语义与 monitor/db 一致：verbosity_level ∈ (minExclusive, max]。
func buildCleanTierDeleteSQL(prefix string, minExclusive, max int, cutoff time.Time) string {
	var b strings.Builder
	b.WriteString("DELETE FROM ")
	b.WriteString(prefix)
	b.WriteString("monitor_tracing WHERE started_at <= ")
	b.WriteString(tsLiteral(cutoff))
	fmt.Fprintf(&b, " AND verbosity_level > %d AND verbosity_level <= %d", minExclusive, max)
	return b.String()
}

// scheduleCleanup 注册 DuckDB 分级清理（tracing.duckdb.cleanup.schedule，
// 默认与 db 一致：每周日 02:11）。
func (s *DuckDBBatchService) scheduleCleanup(logger *zap.Logger) error {
	cron := viper.GetString("tracing.duckdb.cleanup.schedule")
	if cron == "" {
		cron = "11 2 * * 0"
	}
	return cleanup.RegisterScheduledCleanup(logger, "monitor_duckdb_cleanup", cron,
		&tracingCleaner{svc: s}, cleanup.DefaultTracingPolicy())
}
