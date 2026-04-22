package monitor

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/ginshared"
	"go.uber.org/zap"
)

const (
	KeyTracingID = "tracingID"
)

// FullRequestDetails

type TracingDetails struct {
	Optionname     string
	Uri            string
	Method         string
	AppName        string
	AppVersion     string
	VerbosityLevel TracingVerbosityLevel
	Body           []byte
	BodyEnc        string
	Durtion        time.Duration
	Status         int
	TargetID       uint
	Resp           []byte
	RespEnc        string
	ClientIP       string
	UserAgent      string
	Device         string
	Tenant         string
	Operator       string
	StartedAt      time.Time
	// Props     map[string]interface{}
}

type TracingVerbosityLevel int

const (
	TracingVerbosityLevelMostImportant TracingVerbosityLevel = 0
	TracingVerbosityLevelThirdParty    TracingVerbosityLevel = 10
	TracingVerbosityLevelWrite         TracingVerbosityLevel = 50
	TracingVerbosityLevelRead          TracingVerbosityLevel = 99
)

func VerbosityLevelByMethod(method string) TracingVerbosityLevel {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "POST", "PUT", "PATCH", "DELETE":
		return TracingVerbosityLevelWrite
	case "GET", "HEAD", "OPTIONS":
		return TracingVerbosityLevelRead
	default:
		return TracingVerbosityLevelRead
	}
}

const (
	PayloadEncodingUTF8   = "utf8"
	PayloadEncodingBase64 = "base64"
	PayloadEncodingEmpty  = "empty"
)

func DetectPayloadEncoding(payload []byte) string {
	if len(payload) == 0 {
		return PayloadEncodingEmpty
	}
	if utf8.Valid(payload) {
		return PayloadEncodingUTF8
	}
	return PayloadEncodingBase64
}

func EncodePayloadForText(payload []byte) (string, string) {
	encoding := DetectPayloadEncoding(payload)
	switch encoding {
	case PayloadEncodingUTF8:
		return string(payload), encoding
	case PayloadEncodingBase64:
		return base64.StdEncoding.EncodeToString(payload), encoding
	default:
		return "", encoding
	}
}

type RespLogging struct {
	gin.ResponseWriter
	cache *bytes.Buffer
}

func (w RespLogging) Write(b []byte) (int, error) {
	w.cache.Write(b)
	return w.ResponseWriter.Write(b)
}

type TracingRequestService struct {
	Log                    *zap.Logger
	Console                bool
	Request                bool
	Resp                   bool
	Included               []string
	Excluded               []string
	VerbosityLevelByMethod map[string]TracingVerbosityLevel
}

func (tr *TracingRequestService) ShouldLogReq(ctx context.Context, uri string) bool {
	matched := len(tr.Included) == 0
	for _, item := range tr.Included {
		if uri == item {
			matched = true
			break
		}
	}
	if matched {
		for _, item := range tr.Excluded {
			if uri == item {
				return false
			}
		}
	}
	return matched
}

func (tr *TracingRequestService) ResolveVerbosityLevel(method string, fallback TracingVerbosityLevel) TracingVerbosityLevel {
	if tr == nil || len(tr.VerbosityLevelByMethod) == 0 {
		return fallback
	}
	if lvl, ok := tr.VerbosityLevelByMethod[method]; ok {
		return lvl
	}
	trimmed := strings.TrimSpace(method)
	if trimmed != method {
		if lvl, ok := tr.VerbosityLevelByMethod[trimmed]; ok {
			return lvl
		}
	}
	trimmed = strings.TrimPrefix(trimmed, "/")
	if lvl, ok := tr.VerbosityLevelByMethod[trimmed]; ok {
		return lvl
	}
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 && idx+1 < len(trimmed) {
		short := trimmed[idx+1:]
		if lvl, ok := tr.VerbosityLevelByMethod[short]; ok {
			return lvl
		}
	}
	return fallback
}

func VerbosityLevelByGRPCMethod(fullMethod string) TracingVerbosityLevel {
	s := strings.TrimSpace(fullMethod)
	s = strings.TrimPrefix(s, "/")
	method := s
	if idx := strings.LastIndex(s, "/"); idx >= 0 && idx+1 < len(s) {
		method = s[idx+1:]
	}
	m := strings.ToLower(method)
	switch {
	case strings.HasPrefix(m, "create"),
		strings.HasPrefix(m, "update"),
		strings.HasPrefix(m, "delete"),
		strings.HasPrefix(m, "set"),
		strings.HasPrefix(m, "add"),
		strings.HasPrefix(m, "remove"),
		strings.HasPrefix(m, "bind"),
		strings.HasPrefix(m, "unbind"),
		strings.HasPrefix(m, "save"),
		strings.HasPrefix(m, "write"),
		strings.HasPrefix(m, "upsert"),
		strings.HasPrefix(m, "confirm"),
		strings.HasPrefix(m, "cancel"),
		strings.HasPrefix(m, "approve"),
		strings.HasPrefix(m, "submit"):
		return TracingVerbosityLevelWrite
	case strings.HasPrefix(m, "get"),
		strings.HasPrefix(m, "list"),
		strings.HasPrefix(m, "query"),
		strings.HasPrefix(m, "find"),
		strings.HasPrefix(m, "search"),
		strings.HasPrefix(m, "describe"),
		strings.HasPrefix(m, "read"):
		return TracingVerbosityLevelRead
	default:
		return TracingVerbosityLevelRead
	}
}

// func (tr *TracingRequestService) LogRequest(ctx context.Context, req *TracingDetails) error {
// 	// tr.Bus.Publish(core.EventTracing, req)
// 	TracingAdaptor.Push(req)
// 	return nil
// }

var InitTracingService = func(logger *zap.Logger) *TracingRequestService {
	sr := &TracingRequestService{
		Log: logger,
	}

	settings := viper.Sub("tracing")
	if settings == nil {
		logger.Warn("tracing module loaded, but disabled.")
		return sr
	}
	// if settings != nil {
	settings.Unmarshal(sr)
	// }
	logger.Info("tracing service is enabled.")
	if (sr.Request || sr.Resp) && sr.Console {
		c := InitConsoleTracingService(sr.Log)
		TracingAdaptor.Subscripter("console", c.LogBody)
	}

	return sr
}

var TracingAdaptor = core.NewChanAdaptor[TracingDetails](10000)

func init() {
	core.Provide(InitTracingService)
	ginshared.Provide(func(sr *TracingRequestService) ginshared.Component {
		zap.L().Info("tracing service for GIN loaded.")
		comp := &GinTracingService{Service: sr}
		return comp
	}, ginshared.ComponentsOptions)

	// core.ProvideStartup(InitGinComponent)
}

func EnabledTracing() {
}
