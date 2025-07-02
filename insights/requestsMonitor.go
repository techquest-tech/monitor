package insights

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/microsoft/ApplicationInsights-Go/appinsights"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

type AppInsightsSettings struct {
	Key     string
	Role    string
	Version string
	Details bool
}

type ResquestMonitor struct {
	AppInsightsSettings
	logger *zap.Logger
	client appinsights.TelemetryClient
	Locker *sync.Mutex
}

func InitRequestMonitor(logger *zap.Logger) *ResquestMonitor {
	azureSetting := AppInsightsSettings{
		Role:    core.AppName,
		Version: core.Version,
	}
	rm := &ResquestMonitor{
		logger: logger,
		Locker: &sync.Mutex{},
	}
	settings := viper.Sub("tracing.azure")
	if settings != nil {
		settings.Unmarshal(&azureSetting)
	}
	rm.AppInsightsSettings = azureSetting
	if keyFromenv := os.Getenv("APPINSIGHTS_INSTRUMENTATIONKEY"); keyFromenv != "" {
		rm.Key = keyFromenv
		logger.Info("read application insights key from ENV")
	}

	if rm.Key == "" {
		logger.Warn("no application insights key provided, tracing function disabled.")
		return nil
	}

	// bus.SubscribeAsync(event.EventError, client.ReportError, false)
	// bus.SubscribeAsync(event.EventTracing, client.ReportTracing, false)
	// bus.SubscribeAsync(cronext.EventJobFinished, client.ReportScheduleJob, false)
	// logger.Info("event subscribed for application insights", zap.Bool("details", client.Details))
	rm.client = rm.getClient()
	logger.Info("enabled applicationInsights client cache.")
	return rm
}

func (appins *ResquestMonitor) ReportScheduleJob(req schedule.JobHistory) error {
	status := 200
	if !req.Succeed {
		status = 500
	}

	details := monitor.TracingDetails{
		Uri:       req.Job,
		Method:    "Cron",
		Durtion:   req.Duration,
		Status:    status,
		StartedAt: time.Now(),
	}
	appins.ReportTracing(details)
	return nil
}

func (appins *ResquestMonitor) getClient() appinsights.TelemetryClient {
	if appins.client == nil {
		client := appinsights.NewTelemetryClient(appins.Key)
		if appins.Role != "" {
			client.Context().Tags.Cloud().SetRole(appins.Role)
		}
		if appins.Version != "" {
			client.Context().Tags.Application().SetVer(appins.Version)
		}
		appins.client = client
		return client
	}
	return appins.client
}

func (appins *ResquestMonitor) ReportError(rr core.ErrorReport) error {
	appins.Locker.Lock()
	defer appins.Locker.Unlock()

	client := appins.getClient()
	trace := appinsights.NewTraceTelemetry(rr.Error.Error(), appinsights.Error)
	client.Track(trace)
	appins.logger.Debug("tracing error done", zap.Error(rr.Error))
	return nil
}

func (appins *ResquestMonitor) ReportTracing(tr monitor.TracingDetails) error {
	appins.Locker.Lock()
	defer appins.Locker.Unlock()

	client := appins.getClient()

	client.Context().Tags.Operation().SetName(fmt.Sprintf("%s %s", tr.Method, tr.Optionname))

	t := appinsights.NewRequestTelemetry(
		tr.Method, tr.Uri, tr.Durtion, fmt.Sprintf("%d", tr.Status),
	)

	t.Source = tr.ClientIP
	t.Properties["app"] = core.AppName
	t.Properties["version"] = core.Version
	t.Properties["user-agent"] = tr.UserAgent
	t.Properties["device"] = tr.Device
	// if tr.Tenant != "" {
	t.Properties["owner"] = tr.Tenant
	// }
	t.Properties["operator"] = tr.Operator

	// req := monitor.ToByte(tr.Body)
	// resp := monitor.ToByte(tr.Resp)

	if len(tr.Body) > 0 {
		if appins.Details {
			t.Properties["req"] = string(tr.Body)
		}
		t.Measurements["body-size"] = float64(len(tr.Body))
	}
	if len(tr.Resp) > 0 {
		if appins.Details {
			t.Properties["resp"] = string(tr.Resp)
		}
		t.Measurements["resp-size"] = float64(len(tr.Resp))
	}

	client.Track(t)
	appins.logger.Debug("submit tracing done.")
	return nil
}

func EnabledMonitor() {
	core.Provide(InitRequestMonitor)
	// tracing.EnabledTracing()
	core.ProvideStartup(func(logger *zap.Logger, client *ResquestMonitor) core.Startup {
		if client != nil {
			monitor.SubscribeMonitor(logger, client)
		}

		return nil
	})
}
