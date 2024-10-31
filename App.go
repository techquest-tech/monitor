package monitor

import (
	"fmt"

	"github.com/asaskevich/EventBus"
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
	ReportTracing(tr *TracingDetails)
	ReportError(err error)
	ReportScheduleJob(req *schedule.JobHistory)
}

// type P struct {
// 	dig.In
// 	services []MonitorService `group:"monitor"`
// }

func SubscribeMonitor(logger *zap.Logger, bus EventBus.Bus, item MonitorService) {
	bus.SubscribeAsync(core.EventError, item.ReportError, false)
	bus.SubscribeAsync(core.EventTracing, item.ReportTracing, false)
	bus.SubscribeAsync(schedule.EventJobFinished, item.ReportScheduleJob, false)
	logger.Info("sub monitor service", zap.String("service", fmt.Sprintf("%T", item)))

	ch := TracingAdaptor.Subscripter("")
	fn := func() {
		for req := range ch {
			item.ReportTracing(req)
		}
	}
	go fn()
}

// func Enabled() {
// 	core.ProvideStartup(subscribeMonitor)
// }
