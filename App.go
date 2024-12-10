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
	ReportTracing(tr *TracingDetails) error
	ReportError(err error) error
	ReportScheduleJob(req *schedule.JobHistory) error
}

// type P struct {
// 	dig.In
// 	services []MonitorService `group:"monitor"`
// }

func SubscribeMonitor(logger *zap.Logger, item MonitorService) {
	// bus.SubscribeAsync(core.EventError, item.ReportError, false)
	// bus.SubscribeAsync(core.EventTracing, item.ReportTracing, false)
	// bus.SubscribeAsync(schedule.EventJobFinished, item.ReportScheduleJob, false)
	logger.Info("sub monitor service", zap.String("service", fmt.Sprintf("%T", item)))

	TracingAdaptor.Subscripter("", item.ReportTracing)
	schedule.JobHistoryAdaptor.Subscripter("", item.ReportScheduleJob)
	core.ErrorAdaptor.Subscripter("", item.ReportError)
}

// func Enabled() {
// 	core.ProvideStartup(subscribeMonitor)
// }
