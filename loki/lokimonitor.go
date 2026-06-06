package loki

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

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
	MaxBytes    int
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
		MaxBytes:     240 * 1024,
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
	if conf.MaxBytes > 0 {
		loki.MaxBytes = conf.MaxBytes
	}
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
	return lm.pushSplit(header, string(body))
}

func (lm *LokiSetting) ReportError(rr core.ErrorReport) error {
	header := lm.cloneFixedHeader()
	header["dataType"] = "error"
	header["app"] = rr.AppName
	header["version"] = rr.AppVersion
	header["uri"] = rr.Uri
	header["message"] = rr.Error.Error()

	bodyText, bodyEnc := monitor.EncodePayloadForText(rr.FullStack)
	header["stackEnc"] = bodyEnc
	// lm.ch <- lokiclient.NewPushItem(header, body)
	return lm.pushSplit(header, bodyText)
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
	header["VerbosityLevel"] = fmt.Sprintf("%d", tr.VerbosityLevel)
	header["ClientIP"] = tr.ClientIP
	header["UserAgent"] = tr.UserAgent
	header["Device"] = tr.Device
	header["operator"] = tr.Operator
	app := tr.AppName
	if app == "" {
		app = core.AppName
	}
	version := tr.AppVersion
	if version == "" {
		version = core.Version
	}
	header["app"] = app
	header["version"] = version
	header["tenant"] = tr.Tenant
	bodyText, bodyEnc := monitor.EncodePayloadForText(tr.Body)
	respText, respEnc := monitor.EncodePayloadForText(tr.Resp)

	if tr.BodyEnc != "" {
		bodyEnc = tr.BodyEnc
	}
	if tr.RespEnc != "" {
		respEnc = tr.RespEnc
	}

	header["reqEnc"] = bodyEnc
	header["respEnc"] = respEnc

	lokiTr := struct {
		monitor.TracingDetails
		Body string
		Resp string
	}{
		TracingDetails: tr,
		Body:           bodyText,
		Resp:           respText,
	}

	body, err := json.Marshal(lokiTr)
	if err != nil {
		lm.Logger.Error("marshal details failed.", zap.Error(err))
		return err
	}

	// lm.ch <- lokiclient.NewPushItem(header, string(body))
	return lm.pushSplit(header, string(body))
}

func splitUTF8ByBytes(s string, max int) []string {
	if max <= 0 {
		return []string{s}
	}
	b := []byte(s)
	if len(b) <= max {
		return []string{s}
	}
	out := make([]string, 0, (len(b)+max-1)/max)
	for start := 0; start < len(b); {
		end := start + max
		if end >= len(b) {
			out = append(out, string(b[start:]))
			break
		}
		cut := end
		for cut > start {
			_, size := utf8.DecodeLastRune(b[start:cut])
			if size == 1 && b[cut-1] >= 0x80 {
				cut--
				continue
			}
			break
		}
		if cut == start {
			cut = end
		}
		out = append(out, string(b[start:cut]))
		start = cut
	}
	return out
}

func splitWithPrefix(s string, max int) []string {
	parts := splitUTF8ByBytes(s, max)
	if len(parts) <= 1 {
		return parts
	}
	for {
		total := len(parts)
		prefixLen := len(fmt.Sprintf("[part %d/%d] ", total, total))
		if prefixLen >= max {
			return []string{s}
		}
		newMax := max - prefixLen
		newParts := splitUTF8ByBytes(s, newMax)
		if len(newParts) == len(parts) {
			out := make([]string, len(newParts))
			total = len(newParts)
			for i, p := range newParts {
				out[i] = fmt.Sprintf("[part %d/%d] ", i+1, total) + p
			}
			return out
		}
		parts = newParts
	}
}

func (lm *LokiSetting) pushSplit(labels map[string]string, line string) error {
	parts := splitWithPrefix(line, lm.MaxBytes)
	for _, p := range parts {
		err := lm.client.Push(labels, p)
		if err != nil {
			return err
		}
	}
	return nil
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
