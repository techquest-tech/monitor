package loki

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
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

// setLokiLabel 写入 Loki label。
// labels: 目标 label 集合。
// key: 原始 label 名称。
// value: 原始 label 值。
// 返回值：无。
func setLokiLabel(labels map[string]string, key string, value string) {
	normalizedKey := normalizeLokiLabelName(key)
	if normalizedKey == "" {
		return
	}
	trimmedValue := strings.TrimSpace(value)
	if trimmedValue == "" {
		return
	}
	labels[normalizedKey] = trimmedValue
}

// normalizeLokiLabelName 将 label 名规范化为 Loki 可接受的格式。
// key: 原始 label 名称。
// 返回值：返回仅包含字母、数字、下划线，且首字符不为数字的小写 label 名。
func normalizeLokiLabelName(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(key))
	for _, r := range key {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			builder.WriteRune(unicode.ToLower(r))
		case r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}

	normalized := strings.Trim(builder.String(), "_")
	if normalized == "" {
		return ""
	}
	if normalized[0] >= '0' && normalized[0] <= '9' {
		normalized = "_" + normalized
	}
	return normalized
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

	setLokiLabel(loki.FixedHeaders, "app", core.AppName)
	setLokiLabel(loki.FixedHeaders, "version", core.Version)

	// uid := rand.Intn(99999999)
	// loki.FixedHeaders[""] = fmt.Sprintf("%08d", uid)

	hostname, _ := os.Hostname()
	setLokiLabel(loki.FixedHeaders, "hostname", hostname)

	envfile := os.Getenv("ENV")
	if envfile == "" {
		envfile = "default"
	}
	setLokiLabel(loki.FixedHeaders, "env", envfile)

	logger.Info("Loki monitor service is ready.")
	return loki, nil
}

func (lm *LokiSetting) ReportScheduleJob(req schedule.JobHistory) error {
	header := lm.cloneFixedHeader()
	setLokiLabel(header, "data_type", "cron_job")
	setLokiLabel(header, "app", req.App)
	setLokiLabel(header, "succeed", strconv.FormatBool(req.Succeed))
	setLokiLabel(header, "job", req.Job)

	body, _ := json.Marshal(req)
	// lm.ch <- lokiclient.NewPushItem(header, string(body))
	return lm.pushSplit(header, string(body))
}

func (lm *LokiSetting) ReportError(rr core.ErrorReport) error {
	header := lm.cloneFixedHeader()
	setLokiLabel(header, "data_type", "error")
	setLokiLabel(header, "app", rr.AppName)
	setLokiLabel(header, "version", rr.AppVersion)

	bodyText, bodyEnc := monitor.EncodePayloadForText(rr.FullStack)
	setLokiLabel(header, "stack_enc", bodyEnc)
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
	setLokiLabel(header, "data_type", "tracing")
	// optionname 属于业务侧强诉求的查询维度，保留在 label 中便于按功能点聚合检索。
	setLokiLabel(header, "optionname", tr.Optionname)
	setLokiLabel(header, "method", tr.Method)
	setLokiLabel(header, "status", fmt.Sprintf("%d", tr.Status))
	setLokiLabel(header, "verbosity_level", fmt.Sprintf("%d", tr.VerbosityLevel))
	app := tr.AppName
	if app == "" {
		app = core.AppName
	}
	version := tr.AppVersion
	if version == "" {
		version = core.Version
	}
	setLokiLabel(header, "app", app)
	setLokiLabel(header, "version", version)
	setLokiLabel(header, "tenant", tr.Tenant)
	bodyText, _ := monitor.EncodePayloadForText(tr.Body)
	respText, _ := monitor.EncodePayloadForText(tr.Resp)
	// reqEnc/respEnc 不再作为 label 发送，避免 label 数量过多或引入额外维度导致写入被拒绝。
	// 编码信息仍会保留在正文（TracingDetails.BodyEnc/RespEnc）中，便于后续解析与排查。

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
