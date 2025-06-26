package bootup

import (
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

func init() {
	core.InvokeAsyncOnServiceStarted(func() {
		zap.L().Info("init tracing service", zap.Strings("modules", monitor.TracingAdaptor.Receivers()))
	})
}
