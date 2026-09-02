package monitor

import (
	"fmt"

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

// type P struct {
// 	dig.In
// 	services []MonitorService `group:"monitor"`
// }

// subscribeBatch 批量订阅一个 adaptor；若实现不支持批量（如被换成非批量
// 实现），回退为逐条调用同一个批量 handler（每条包成单元素 batch），保证
// 调用方无需感知底层能力差异。
func subscribeBatch[T any](adaptor core.Adaptor[T], receiver string, fn core.BatchHandler[T]) {
	if b, ok := adaptor.(core.BatchingAdaptor[T]); ok {
		b.SubscripterBatch(receiver, core.DefaultBatchSize, core.DefaultBatchInterval, fn)
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
			subscribeBatch(TracingAdaptor, receiver, func(trs []TracingDetails) error {
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
			subscribeBatch(TracingAdaptor, receiver, b.ReportTracingBatch)
		}
		subscribeBatch(schedule.JobHistoryAdaptor, receiver, b.ReportScheduleJobBatch)
		subscribeBatch(core.ErrorAdaptor, receiver, b.ReportErrorBatch)
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
