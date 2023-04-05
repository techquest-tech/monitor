package monitor

import (
	"fmt"

	"github.com/asaskevich/EventBus"
	"github.com/techquest-tech/cronext"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/event"
	"github.com/techquest-tech/gin-shared/pkg/tracing"
	"go.uber.org/zap"
)

type AppSettings struct {
	Appname string
	Version string
	Details bool
}

type MonitorService interface {
	ReportTracing(tr *tracing.TracingDetails)
	ReportError(err error)
	ReportScheduleJob(req cronext.JobHistory)
}

func subscribeMonitor(logger *zap.Logger, bus EventBus.Bus, services []MonitorService) core.Startup {
	if len(services) == 0 {
		logger.Info("no monitor service provided.")
		return nil
	}
	for _, item := range services {
		bus.SubscribeAsync(event.EventError, item.ReportError, false)
		bus.SubscribeAsync(event.EventTracing, item.ReportTracing, false)
		bus.SubscribeAsync(cronext.EventJobFinished, item.ReportScheduleJob, false)
		logger.Info("sub monitor service", zap.String("service", fmt.Sprintf("%T", item)))
	}

	return nil
}

func init() {
	core.ProvideStartup(subscribeMonitor)
}
