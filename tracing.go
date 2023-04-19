package monitor

import (
	"bytes"
	"io"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/event"
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
	Body       any
	Durtion    time.Duration
	Status     int
	TargetID   uint
	Resp       any
	ClientIP   string
	UserAgent  string
	Device     string
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
	Bus      EventBus.Bus
	Log      *zap.Logger
	Console  bool
	Request  bool
	Resp     bool
	Included []string
	Excluded []string
}

func (tr *TracingRequestService) Priority() int { return 5 }

func (tr *TracingRequestService) OnEngineInited(r *gin.Engine) error {
	r.Use(tr.LogfullRequestDetails)
	return nil
}

var InitTracingService = func(bus EventBus.Bus, logger *zap.Logger) *TracingRequestService {
	sr := &TracingRequestService{
		Bus: bus,
		Log: logger,
	}

	settings := viper.Sub("tracing")
	if settings == nil {
		logger.Warn("tracing module loaded, but disabled.")
		return nil
	}
	// if settings != nil {
	settings.Unmarshal(sr)
	// }
	logger.Info("tracing service is enabled.")
	if (sr.Request || sr.Resp) && sr.Console {
		c := InitConsoleTracingService(sr.Log)
		sr.Bus.SubscribeAsync(event.EventTracing, c.LogBody, false)
	}
	return sr
}

func (tr *TracingRequestService) LogfullRequestDetails(c *gin.Context) {
	start := time.Now()
	reqcache := make([]byte, 0)

	uri := c.Request.RequestURI
	method := c.Request.Method

	matchedUrl := c.FullPath()
	if matchedUrl == "" {
		tr.Log.Warn("matched path failed. use uri as matched url", zap.String("uri", uri))
		matchedUrl = uri
	}

	matched := len(tr.Included) == 0
	for _, item := range tr.Included {
		if matchedUrl == item {
			matched = true
			break
		}
	}
	if matched {
		for _, item := range tr.Excluded {
			if matchedUrl == item {
				matched = false
				break
			}
		}
	}

	if tr.Request && matched {
		if c.Request.Body != nil {
			reqcache, _ = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(reqcache))
		}
	}

	// respcache := make([]byte, 0)
	writer := &RespLogging{
		cache:          bytes.NewBuffer([]byte{}),
		ResponseWriter: c.Writer,
	}

	if tr.Resp && matched {
		c.Writer = writer
	}

	c.Next()

	dur := time.Since(start)

	status := c.Writer.Status()
	rawID := c.GetUint(KeyTracingID)

	respcache := writer.cache.Bytes()

	fullLogging := &TracingDetails{
		Optionname: matchedUrl,
		Uri:        uri,
		Method:     method,
		Body:       string(reqcache),
		Durtion:    dur,
		Status:     status,
		TargetID:   rawID,
		Resp:       string(respcache),
		ClientIP:   c.ClientIP(),
		UserAgent:  c.Request.UserAgent(),
		Device:     c.GetHeader("deviceID"),
	}

	tr.Bus.Publish(event.EventTracing, fullLogging)
}

func EnabledTracing() {
	core.Provide(InitTracingService)
	core.ProvideStartup(func(p core.OptionalParam[*TracingRequestService]) core.Startup {
		return p.P
	})
	core.GetContainer().Invoke(func(p core.OptionalParam[*TracingRequestService]) {
		if p.P != nil {
			ginshared.RegisterComponent(p.P)
			zap.L().Info("tracning is enabled.")
		}
	})
}
