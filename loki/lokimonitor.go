package loki

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

type LokiConfig struct {
	URL         string
	User        string
	Password    string
	Protocol    string
	Included    []string
	Excluded    []string
	IncludedIPs []string
	ExcludedIPs []string
}

type LokiSetting struct {
	monitor.BaseFilter
	// monitor.AppSettings
	Config       *LokiConfig
	FixedHeaders map[string]string
	Logger       *zap.Logger
	MaxBytes     int
	client       LokiClient
}

type LokiClient interface {
	Push(labels map[string]string, line string) error
	Close() error
}

func InitLokiMonitor(logger *zap.Logger) (*LokiSetting, error) {
	loki := &LokiSetting{
		FixedHeaders: map[string]string{},
		Logger:       logger,
		MaxBytes:     262144,
	}
	// settings := viper.Sub("tracing.loki")
	// if settings == nil {
	// 	logger.Info("no loki client config. return nil")
	// 	return nil, nil
	// }
	conf := &LokiConfig{}
	// logger.Info("connect to loki", zap.String("loki", settings.GetString("URL")))
	err := viper.UnmarshalKey("tracing.loki", conf) //settings.Unmarshal(conf)
	if err != nil {
		logger.Error("loki config error.", zap.Error(err))
		return nil, err
	}

	if conf.URL == "" {
		logger.Info("no loki client config, return nil")
		return nil, nil
	}

	loki.Config = conf
	loki.BaseFilter = monitor.BaseFilter{
		Included: conf.Included,
		Excluded: conf.Excluded,
	}

	// Choose client by config; default REST
	var client LokiClient
	proto := strings.ToLower(strings.TrimSpace(conf.Protocol))
	if proto == "" || proto == "rest" {
		rc, rerr := NewRestClient(conf)
		if rerr != nil {
			logger.Warn("init loki REST client failed, try gRPC", zap.Error(rerr))
			gc, gerr := NewGrpcClient(conf)
			if gerr != nil {
				logger.Error("init loki gRPC client failed", zap.Error(gerr))
				return nil, gerr
			}
			client = gc
		} else {
			client = rc
		}
	} else {
		gc, gerr := NewGrpcClient(conf)
		if gerr != nil {
			logger.Warn("init loki gRPC client failed, try REST", zap.Error(gerr))
			rc, rerr := NewRestClient(conf)
			if rerr != nil {
				logger.Error("init loki REST client failed", zap.Error(rerr))
				return nil, rerr
			}
			client = rc
		} else {
			client = gc
		}
	}
	loki.client = client
	used := "grpc"
	if _, ok := client.(*RestClient); ok {
		used = "rest"
	}
	logger.Info("connect to loki",
		zap.String("protocol", used),
		zap.String("url", conf.URL),
		zap.String("user", conf.User),
		zap.Bool("hasAuth", conf.User != "" || conf.Password != ""),
		zap.Strings("included", conf.Included),
		zap.Strings("excluded", conf.Excluded),
	)

	core.OnServiceStopping(func() {
		client.Close()
	})

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

func (lm *LokiSetting) ReportScheduleJob(req schedule.JobHistory) error {
	header := lm.cloneFixedHeader()
	header["dataType"] = "cronJob"
	header["app"] = req.App
	header["succeed"] = strconv.FormatBool(req.Succeed)
	header["job"] = req.Job

	body, _ := json.Marshal(req)
	// lm.ch <- lokiclient.NewPushItem(header, string(body))
	return lm.client.Push(header, string(body))
}

func (lm *LokiSetting) ReportError(rr core.ErrorReport) error {
	header := lm.cloneFixedHeader()
	header["dataType"] = "error"
	header["app"] = core.AppName
	header["version"] = core.Version
	header["uri"] = rr.Uri
	header["message"] = rr.Error.Error()

	body := string(rr.FullStack)
	// lm.ch <- lokiclient.NewPushItem(header, body)
	return lm.client.Push(header, body)
}

func (lm *LokiSetting) cloneFixedHeader() map[string]string {
	out := map[string]string{}
	for k, v := range lm.FixedHeaders {
		out[k] = v
	}
	return out
}

func (lm *LokiSetting) ReportTracing(tr monitor.TracingDetails) error {
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
		return err
	}

	if len(body) > lm.MaxBytes {
		lm.Logger.Info("body truncated.", zap.Int("raw", len(body)))
		body = body[:lm.MaxBytes-1]
	}

	// lm.ch <- lokiclient.NewPushItem(header, string(body))
	return lm.client.Push(header, string(body))
}

func EnableLokiMonitor() {
	core.Provide(InitLokiMonitor)
	core.ProvideStartup(func(logger *zap.Logger, s *LokiSetting) core.Startup {
		if s != nil {
			monitor.SubscribeMonitor(logger, s)
		}

		return nil
	})
}
