package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/asaskevich/EventBus"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/lokiclient"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

type LokiSetting struct {
	// monitor.AppSettings
	Config       *lokiclient.PushConfig
	FixedHeaders map[string]string
	Logger       *zap.Logger
	MaxBytes     int
	ch           chan any
}

func InitLokiMonitor(logger *zap.Logger) (*LokiSetting, error) {
	loki := &LokiSetting{
		FixedHeaders: map[string]string{},
		Logger:       logger,
		MaxBytes:     262144,
	}
	settings := viper.Sub("tracing.loki")
	if settings == nil {
		logger.Info("no loki client config. return nil")
		return nil, nil
	}
	conf := &lokiclient.PushConfig{
		URL:      "http://127.0.0.1:3001",
		Batch:    100,
		Interval: "10s",
		Gzip:     true,
	}
	// logger.Info("connect to loki", zap.String("loki", settings.GetString("URL")))
	err := settings.Unmarshal(conf)
	if err != nil {
		logger.Error("loki config error.", zap.Error(err))
		return nil, err
	}
	loki.Config = conf

	//random an ID
	// rand.Seed(time.Now().Unix())

	ctx := context.TODO()
	ch, err := conf.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	loki.ch = ch

	loki.FixedHeaders["app"] = core.AppName
	loki.FixedHeaders["version"] = core.Version

	// uid := rand.Intn(99999999)
	// loki.FixedHeaders[""] = fmt.Sprintf("%08d", uid)

	hostname, _ := os.Hostname()
	loki.FixedHeaders["hostname"] = hostname

	envfile := os.Getenv("ENV")
	if envfile == "" {
		envfile = "default"
	}
	loki.FixedHeaders["ENV"] = envfile

	logger.Info("Loki monitor service is ready.")
	return loki, nil
}

func (lm *LokiSetting) ReportScheduleJob(req *schedule.JobHistory) {
	header := lm.cloneFixedHeader()
	header["dataType"] = "cronJob"
	header["job"] = req.Job

	body, _ := json.Marshal(req)
	lm.ch <- lokiclient.NewPushItem(header, string(body))
}

func (lm *LokiSetting) ReportError(err error) {
	header := lm.cloneFixedHeader()
	header["dataType"] = "error"
	header["app"] = core.AppName
	header["version"] = core.Version

	body := err.Error()
	lm.ch <- lokiclient.NewPushItem(header, body)
}

func (lm *LokiSetting) cloneFixedHeader() map[string]string {
	out := map[string]string{}
	for k, v := range lm.FixedHeaders {
		out[k] = v
	}
	return out
}

func (lm *LokiSetting) ReportTracing(tr *monitor.TracingDetails) {
	header := lm.cloneFixedHeader()
	header["dataType"] = "tracing"
	header["Optionname"] = tr.Optionname
	header["URI"] = tr.Uri
	header["Method"] = tr.Method
	header["Status"] = fmt.Sprintf("%d", tr.Status)
	header["ClientIP"] = tr.ClientIP
	header["UserAgent"] = tr.UserAgent
	header["Device"] = tr.Device
	header["operator"] = tr.Operator
	header["app"] = core.AppName
	header["version"] = core.Version
	header["tenant"] = tr.Tenant

	body, err := json.Marshal(tr)
	if err != nil {
		lm.Logger.Error("marshal details failed.", zap.Error(err))
		return
	}

	if len(body) > lm.MaxBytes {
		lm.Logger.Info("body truncated.", zap.Int("raw", len(body)))
		body = body[:lm.MaxBytes-1]
	}

	lm.ch <- lokiclient.NewPushItem(header, string(body))
}

func EnableLokiMonitor() {
	core.Provide(InitLokiMonitor)
	core.ProvideStartup(func(logger *zap.Logger, bus EventBus.Bus, s *LokiSetting) core.Startup {
		if s != nil {
			monitor.SubscribeMonitor(logger, bus, s)
		}

		return nil
	})
}
