package monitor

import (
	"bytes"
	"io"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"go.uber.org/zap"
)

const (
	// ginContextKeyCurrentUser 兼容 gin-shared/auth 中间件写入的当前用户上下文键。
	// 这里直接使用常量，避免仅因 import auth 包触发其 init() 注册数据库实体。
	ginContextKeyCurrentUser = "currentUser"
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

// extractTracingUser 从 gin 上下文提取租户和操作人。
// c: 当前请求上下文。
// 返回值：tenant 为租户，operator 为操作人；当未命中时返回空字符串。
func extractTracingUser(c *gin.Context) (tenant string, operator string) {
	if obj, ok := c.Get(ginContextKeyCurrentUser); ok {
		value := reflect.ValueOf(obj)
		if value.IsValid() {
			if value.Kind() == reflect.Pointer {
				if value.IsNil() {
					return "", ""
				}
				value = value.Elem()
			}
			// 兼容 *auth.AuthKey 等结构体对象，但不直接依赖具体包类型。
			if value.Kind() == reflect.Struct {
				ownerField := value.FieldByName("Owner")
				if ownerField.IsValid() && ownerField.Kind() == reflect.String {
					tenant = ownerField.String()
				}
				userNameField := value.FieldByName("UserName")
				if userNameField.IsValid() && userNameField.Kind() == reflect.String {
					operator = userNameField.String()
				}
				if tenant != "" || operator != "" {
					return tenant, operator
				}
			}
		}
	}

	return c.GetString("owner"), c.GetString("user")
}

// LogfullRequestDetails 记录 gin 请求与响应的 tracing 详情。
// c: 当前请求上下文。
// 返回值：无。
func (tr *GinTracingService) LogfullRequestDetails(c *gin.Context) {
	startAt := time.Now()
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
		if c.Request.Body != nil { //TODO: check content-length, if too large, should skip
			reqcache, _ = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(reqcache))
			ct := c.Request.Header.Get("Content-Type")
			if ct == "application/x-www-form-urlencoded" {
				reqboy, err := url.QueryUnescape(string(reqcache))
				if err == nil {
					reqcache = []byte(reqboy)
				}
			}
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

	fullLogging := TracingDetails{
		Optionname:     matchedUrl,
		Uri:            uri,
		Method:         method,
		AppName:        core.AppName,
		AppVersion:     core.Version,
		VerbosityLevel: VerbosityLevelByMethod(method),
		Body:           reqcache,
		BodyEnc:        DetectPayloadEncoding(reqcache),
		Durtion:        dur,
		Status:         status,
		TargetID:       rawID,
		Resp:           respcache,
		RespEnc:        DetectPayloadEncoding(respcache),
		ClientIP:       c.ClientIP(),
		UserAgent:      c.Request.UserAgent(),
		Device:         c.GetHeader("deviceID"),
		StartedAt:      startAt,
	}

	tenant, operator := extractTracingUser(c)
	if tenant != "" {
		fullLogging.Tenant = tenant
	}
	if operator != "" {
		fullLogging.Operator = operator
	}

	TracingAdaptor.Push(fullLogging)
}
