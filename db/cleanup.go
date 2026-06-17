package db

import (
	"time"

	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

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

		err := schedule.CreateSchedule("monitor_db_cleanup", scheduleStr, func() {
			now := time.Now()

			cutoff0to10 := now.AddDate(0, -6, 0)
			cutoff10to50 := now.AddDate(0, 0, -14)
			cutoff50plus := now.AddDate(0, 0, -3)

			result := db.Unscoped().
				Where("created_at <= ? AND verbosity_level <= ?", cutoff0to10, 10).
				Delete(&FullRequestDetails{})
			if result.Error != nil {
				logger.Error("delete data failed", zap.Error(result.Error), zap.Int("minLevel", 0), zap.Int("maxLevel", 10))
				return
			}

			result = db.Unscoped().
				Where("created_at <= ? AND verbosity_level > ? AND verbosity_level <= ?", cutoff10to50, 10, 50).
				Delete(&FullRequestDetails{})
			if result.Error != nil {
				logger.Error("delete data failed", zap.Error(result.Error), zap.Int("minLevel", 11), zap.Int("maxLevel", 50))
				return
			}

			result = db.Unscoped().
				Where("created_at <= ? AND verbosity_level > ?", cutoff50plus, 50).
				Delete(&FullRequestDetails{})
			if result.Error != nil {
				logger.Error("delete data failed", zap.Error(result.Error), zap.Int("minLevel", 51))
				return
			}
		})
		if err != nil {
			logger.Error("schedule db cleanup job failed", zap.Error(err))
		}
		return nil
	})
}
