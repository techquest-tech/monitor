package db

import (
	"context"
	"time"

	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/monitor/cleanup"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// tracingCleaner 实现 cleanup.Cleaner：按 verbosity 分档删除 FullRequestDetails
// （时间列 created_at，gorm.Model 的创建时间）。
type tracingCleaner struct {
	db *gorm.DB
}

// CleanTier 删除 created_at <= cutoff 且 verbosity_level ∈ (minExclusive, max] 的记录。
func (c *tracingCleaner) CleanTier(ctx context.Context, minExclusive, max int, cutoff time.Time) (int64, error) {
	result := c.db.WithContext(ctx).Unscoped().
		Where("created_at <= ? AND verbosity_level > ? AND verbosity_level <= ?", cutoff, minExclusive, max).
		Delete(&FullRequestDetails{})
	return result.RowsAffected, result.Error
}

func init() {
	core.ProvideStartup(func(logger *zap.Logger, db *gorm.DB) core.Startup {
		settings := viper.Sub("tracing.db.cleanup")
		scheduleStr := ""
		if settings != nil {
			scheduleStr = settings.GetString("schedule")
		}
		if scheduleStr == "" {
			scheduleStr = "11 2 * * 0"
		}

		// 分级保留策略与调度由共用模块 monitor/cleanup 统一提供
		// （verbosity ≤10 → 6 个月；11-50 → 14 天；>50 → 3 天）。
		err := cleanup.RegisterScheduledCleanup(logger, "monitor_db_cleanup", scheduleStr,
			&tracingCleaner{db: db}, cleanup.DefaultTracingPolicy())
		if err != nil {
			logger.Error("schedule db cleanup job failed", zap.Error(err))
		}
		return nil
	})
}
