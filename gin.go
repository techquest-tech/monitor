package monitor

import (
	"bytes"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/techquest-tech/gin-shared/pkg/auth"
	"go.uber.org/zap"
)

type GinTracingService struct {
	Service *TracingRequestService
}

func (tr *GinTracingService) Priority() int { return 5 }

func (tr *GinTracingService) OnEngineInited(r *gin.Engine) error {
	zap.L().Info("tracing gin middleware ready.")
	r.Use(tr.LogfullRequestDetails)
	return nil
}

// func InitGinComponent(service *TracingRequestService) core.Startup {
// 	return nil
// }

func (tr *GinTracingService) LogfullRequestDetails(c *gin.Context) {
	userAgent := c.Request.UserAgent()
	if strings.HasPrefix(userAgent, "kube-probe") {
		c.Next()
		return
	}

	start := time.Now()
	reqcache := make([]byte, 0)

	uri := c.Request.RequestURI
	method := c.Request.Method

	matchedUrl := c.FullPath()
	if matchedUrl == "" {
		tr.Service.Log.Warn("matched path failed. use uri as matched url", zap.String("uri", uri))
		matchedUrl = uri
	}

	matched := tr.Service.ShouldLogReq(c.Request.Context(), matchedUrl)
	if matched {
		for _, item := range tr.Service.Excluded {
			if matchedUrl == item {
				matched = false
				break
			}
		}
	}

	if tr.Service.Request && matched {
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

	if tr.Service.Resp && matched {
		c.Writer = writer
	}

	c.Next()

	dur := time.Since(start)

	status := c.Writer.Status()
	rawID := c.GetUint(KeyTracingID)

	respcache := writer.cache.Bytes()

	if index := strings.IndexRune(matchedUrl, '?'); index > 0 {
		matchedUrl = matchedUrl[:index]
	}

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

	if obj, ok := c.Get(auth.KeyUser); ok {
		currentUser := obj.(*auth.AuthKey)
		fullLogging.Tenant = currentUser.Owner
		fullLogging.Operator = currentUser.UserName
	}

	TracingAdaptor.Push(fullLogging)
}