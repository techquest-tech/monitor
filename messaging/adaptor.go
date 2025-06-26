package messaging

import (
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/messaging"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

var (
	tracing = &messaging.MessagingAdaptor[monitor.TracingDetails]{
		Topic:        "monitor.tracing",
		ChanAdaaptor: monitor.TracingAdaptor,
	}

	jobReport = &messaging.MessagingAdaptor[schedule.JobHistory]{
		Topic:        "monitor.schedule",
		ChanAdaaptor: schedule.JobHistoryAdaptor,
	}
	errorReport = &messaging.MessagingAdaptor[core.ErrorReport]{
		Topic:        "monitor.error",
		ChanAdaaptor: core.ErrorAdaptor,
	}
)

// redis monitor bridge, monitor data to redis first, then via redis adaptor to real monitor persist.
func EnabledMessagingBridge() {
	core.ProvideStartup(func(logger *zap.Logger, service messaging.MessagingService) core.Startup {
		tracing.AsBridge(service)
		jobReport.AsBridge(service)
		errorReport.AsBridge(service)

		zap.L().Info("messaging service as bridge enabled")
		return nil
	})
}

func RunAsAdaptor() error {
	return core.GetContainer().Invoke(func(logger *zap.Logger, service messaging.MessagingService, _ core.Startups) {
		ctx := core.RootCtx()
		consumer := "monitor-adaptor"

		service.Sub(ctx, tracing.Topic, consumer, tracing.Adaptor)
		service.Sub(ctx, jobReport.Topic, consumer, jobReport.Adaptor)
		service.Sub(ctx, errorReport.Topic, consumer, errorReport.Adaptor)
		zap.L().Info("messaging service as adaptor enabled")
	})
}
