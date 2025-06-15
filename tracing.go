package monitor

import (
	"bytes"
	"context"
	"math"
	"time"

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
	Optionname string
	Uri        string
	Method     string
	Body       string
	Durtion    time.Duration
	Status     int
	TargetID   uint
	Resp       string
	ClientIP   string
	UserAgent  string
	Device     string
	Tenant     string
	Operator   string
	// Props     map[string]interface{}
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
	Log      *zap.Logger
	Console  bool
	Request  bool
	Resp     bool
	Included []string
	Excluded []string
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

var TracingAdaptor = core.NewChanAdaptor[TracingDetails](math.MaxInt16)

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
	// core.InvokeAsyncOnServiceStarted(TracingAdaptor.Start)
	// core.InvokeAsyncOnServiceStarted(schedule.JobHistoryAdaptor.Start)
	// core.InvokeAsyncOnServiceStarted(core.ErrorAdaptor.Start)
}
