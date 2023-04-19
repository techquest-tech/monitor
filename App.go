package monitor

import (
	"fmt"

	"github.com/asaskevich/EventBus"
	"github.com/techquest-tech/cronext"
	"github.com/techquest-tech/gin-shared/pkg/event"
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
	ReportScheduleJob(req cronext.JobHistory)
}

// type P struct {
// 	dig.In
// 	services []MonitorService `group:"monitor"`
// }

func SubscribeMonitor(logger *zap.Logger, bus EventBus.Bus, item MonitorService) {
	bus.SubscribeAsync(event.EventError, item.ReportError, false)
	bus.SubscribeAsync(event.EventTracing, item.ReportTracing, false)
	bus.SubscribeAsync(cronext.EventJobFinished, item.ReportScheduleJob, false)
	logger.Info("sub monitor service", zap.String("service", fmt.Sprintf("%T", item)))
}

// func Enabled() {
// 	core.ProvideStartup(subscribeMonitor)
// }
