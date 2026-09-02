// Package cleanup 提供 monitor 数据的分级保留清理（共用模块）。
//
// 语义与 monitor/db 原有清理一致：按 verbosity_level 分档保留——
//
//	verbosity ≤ 10  → 保留 6 个月
//	10 < v ≤ 50    → 保留 14 天
//	v > 50         → 保留 3 天
//
// 后端只需实现 Cleaner（按档执行删除），调度与策略计算由本模块统一提供；
// MySQL（monitor/db）与 DuckDB（monitor/duckdb）复用同一套逻辑。
package cleanup

import (
	"context"
	"math"
	"time"

	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"go.uber.org/zap"
)

// Tier 一个保留档位：verbosity_level ∈ (上一档 Max, Max] 的记录保留
// Months 个自然月 + Days 天（与 todb 原有 AddDate 日历语义一致）。
type Tier struct {
	Max    int // 本档 verbosity 上限（含）；最后档用 MaxInt 表示无上限
	Months int
	Days   int
}

// Policy 保留策略（按档升序排列）。
type Policy struct {
	Tiers []Tier
}

// DefaultTracingPolicy 与 monitor/db 清理一致的默认策略：
// 0-10 → 6 个月；11-50 → 14 天；>50 → 3 天。
func DefaultTracingPolicy() Policy {
	return Policy{Tiers: []Tier{
		{Max: 10, Months: 6},
		{Max: 50, Days: 14},
		{Max: math.MaxInt, Days: 3},
	}}
}

// CleanResult 单档删除结果。
type CleanResult struct {
	MinVerbosity int
	MaxVerbosity int
	Cutoff       time.Time
	Deleted      int64
	Err          error
}

// Cleaner 由后端实现：删除 verbosity_level ∈ (minExclusive, max] 且时间列
// <= cutoff 的记录，返回删除行数。首档请传 minExclusive=-1（含 verbosity=0）。
type Cleaner interface {
	CleanTier(ctx context.Context, minExclusive, max int, cutoff time.Time) (int64, error)
}

// Run 执行一次完整分级清理，返回每档结果。
func Run(ctx context.Context, c Cleaner, p Policy) []CleanResult {
	results := make([]CleanResult, 0, len(p.Tiers))
	prev := -1 // 首档无下界：verbosity_level > -1 恒成立（最小值 0），等价原 todb 的 <= Max
	now := time.Now()
	for _, t := range p.Tiers {
		cutoff := now.AddDate(0, -t.Months, -t.Days)
		n, err := c.CleanTier(ctx, prev, t.Max, cutoff)
		results = append(results, CleanResult{
			MinVerbosity: prev,
			MaxVerbosity: t.Max,
			Cutoff:       cutoff,
			Deleted:      n,
			Err:          err,
		})
		prev = t.Max
	}
	return results
}

// RegisterScheduledCleanup 注册定时清理任务（复用 gin-shared schedule）。
// cron 为空或 "-" 时任务被禁用（schedule 自身语义）。
func RegisterScheduledCleanup(logger *zap.Logger, jobName, cron string, c Cleaner, p Policy) error {
	return schedule.CreateSchedule(jobName, cron, func() {
		results := Run(context.Background(), c, p)
		for _, r := range results {
			if r.Err != nil {
				logger.Error("cleanup tier failed",
					zap.String("job", jobName),
					zap.Int("minVerbosity", r.MinVerbosity),
					zap.Int("maxVerbosity", r.MaxVerbosity),
					zap.Time("cutoff", r.Cutoff),
					zap.Error(r.Err))
			} else {
				logger.Info("cleanup tier done",
					zap.String("job", jobName),
					zap.Int("minVerbosity", r.MinVerbosity),
					zap.Int("maxVerbosity", r.MaxVerbosity),
					zap.Time("cutoff", r.Cutoff),
					zap.Int64("deleted", r.Deleted))
			}
		}
	})
}
