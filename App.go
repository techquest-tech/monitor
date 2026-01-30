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

type Filterable interface {
	ShouldFilter(tr TracingDetails) bool
}

// type P struct {
// 	dig.In
// 	services []MonitorService `group:"monitor"`
// }

func SubscribeMonitor(logger *zap.Logger, item MonitorService) {
	receiver := fmt.Sprintf("%T", item)
	logger.Info("sub monitor service", zap.String("service", receiver))

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
