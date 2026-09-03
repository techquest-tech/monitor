package monitor

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"go.uber.org/zap"
)

type AppSettings struct {
	Appname string
	Version string
	Details bool
}

type MonitorService interface {
	ReportTracing(tr TracingDetails) error
	ReportError(core.ErrorReport) error
	ReportScheduleJob(req schedule.JobHistory) error
}

// MonitorBatchService is the optional batched variant of MonitorService.
// Services that implement it are subscribed via batched delivery: one handler
// call per batch (instead of a goroutine per message), and — when the adaptor
// is redis-backed — a batch is only acknowledged after the handler returns nil,
// so a full loki queue or a failed write keeps the data pending for redelivery
// instead of silently losing it.
type MonitorBatchService interface {
	MonitorService
	ReportTracingBatch(trs []TracingDetails) error
	ReportErrorBatch([]core.ErrorReport) error
	ReportScheduleJobBatch(reqs []schedule.JobHistory) error
}

type Filterable interface {
	ShouldFilter(tr TracingDetails) bool
}

// batchConfig resolves the batch size and flush interval for one batch
// subscription from the config section named by prefix (e.g. "tracing.batch",
// "schedule.batch", "error.batch"), falling back to the gin-shared defaults
// (core.DefaultBatchSize=200, core.DefaultBatchInterval=30s).
func batchConfig(prefix string) (int, time.Duration) {
	size := core.DefaultBatchSize
	interval := core.DefaultBatchInterval
	if viper.IsSet(prefix+".size") && viper.GetInt(prefix+".size") > 0 {
		size = viper.GetInt(prefix + ".size")
	}
	if viper.IsSet(prefix+".interval") && viper.GetDuration(prefix+".interval") > 0 {
		interval = viper.GetDuration(prefix + ".interval")
	}
	return size, interval
}

// subscribeJitter returns a random 1-5s delay applied when a batch subscription
// starts listening. Staggering each start avoids consumers that share the same
// flushInterval (the XReadGroup Block) all timing out and writing at the same
// moment.
func subscribeJitter() time.Duration {
	return time.Second + time.Duration(rand.Int64N(int64(4*time.Second)))
}

// SubscribeBatch subscribes one adaptor to a batch handler. batchSize /
// flushInterval are resolved from config (prefix+".size" / prefix+".interval")
// with core defaults as fallback, and the subscription start is staggered by a
// random 1-5s jitter. When the adaptor does not support batching, it falls back
// to single-message delivery through the same batch handler (each message
// wrapped as a one-element batch).
func SubscribeBatch[T any](adaptor core.Adaptor[T], receiver, prefix string, fn core.BatchHandler[T]) {
	if b, ok := adaptor.(core.BatchingAdaptor[T]); ok {
		size, interval := batchConfig(prefix)
		time.Sleep(subscribeJitter())
		b.SubscripterBatch(receiver, size, interval, fn)
		return
	}
	adaptor.Subscripter(receiver, func(v T) error {
		return fn([]T{v})
	})
}

func SubscribeMonitor(logger *zap.Logger, item MonitorService) {
	receiver := fmt.Sprintf("%T", item)
	logger.Info("sub monitor service", zap.String("service", receiver))

	if b, ok := item.(MonitorBatchService); ok {
		if f, ok2 := item.(Filterable); ok2 {
			SubscribeBatch(TracingAdaptor, receiver, "tracing.batch", func(trs []TracingDetails) error {
				kept := trs[:0]
				for _, tr := range trs {
					if f.ShouldFilter(tr) {
						kept = append(kept, tr)
					}
				}
				if len(kept) == 0 {
					return nil
				}
				return b.ReportTracingBatch(kept)
			})
		} else {
			SubscribeBatch(TracingAdaptor, receiver, "tracing.batch", b.ReportTracingBatch)
		}
		SubscribeBatch(schedule.JobHistoryAdaptor, receiver, "schedule.batch", b.ReportScheduleJobBatch)
		SubscribeBatch(core.ErrorAdaptor, receiver, "error.batch", b.ReportErrorBatch)
		return
	}

	if f, ok := item.(Filterable); ok {
		TracingAdaptor.Subscripter(receiver, func(tr TracingDetails) error {
			if f.ShouldFilter(tr) {
				return item.ReportTracing(tr)
			}
			return nil
		})
	} else {
		TracingAdaptor.Subscripter(receiver, item.ReportTracing)
	}

	schedule.JobHistoryAdaptor.Subscripter(receiver, item.ReportScheduleJob)
	core.ErrorAdaptor.Subscripter(receiver, item.ReportError)
}
