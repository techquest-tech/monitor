package monitor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"go.uber.org/zap"
)

// recorderAdaptor 记录 Push 的 tracing，不启动投递 goroutine。
type recorderAdaptor struct {
	pushes []TracingDetails
}

func (r *recorderAdaptor) Push(data TracingDetails) { r.pushes = append(r.pushes, data) }
func (r *recorderAdaptor) Sub(receiver string) <-chan TracingDetails {
	return nil
}
func (r *recorderAdaptor) Subscripter(receiver string, fn core.Handler[TracingDetails]) {}
func (r *recorderAdaptor) Start()                                                       {}
func (r *recorderAdaptor) Stop()                                                        {}
func (r *recorderAdaptor) Receivers() []string                                          { return nil }

var _ core.Adaptor[TracingDetails] = (*recorderAdaptor)(nil)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func resetTracingConfig(t *testing.T) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
}

func TestTracingEnabledDefaultsToTrue(t *testing.T) {
	resetTracingConfig(t)

	assert.True(t, TracingEnabled(), "未配置 tracing.enabled 时应默认开启")

	viper.Set("tracing.enabled", false)
	assert.False(t, TracingEnabled())

	viper.Set("tracing.enabled", true)
	assert.True(t, TracingEnabled())
}

func TestShouldLogReqRespectsTracingSwitch(t *testing.T) {
	resetTracingConfig(t)

	svc := &TracingRequestService{}
	assert.True(t, svc.ShouldLogReq(context.Background(), "/api/test"))

	// gRPC 拦截器等实现用 ShouldLogReq 判断是否采集，关闭后必须同样返回 false。
	viper.Set("tracing.enabled", false)
	assert.False(t, svc.ShouldLogReq(context.Background(), "/api/test"))
}

func TestGinMiddlewareSkippedWhenTracingDisabled(t *testing.T) {
	resetTracingConfig(t)
	gin.SetMode(gin.TestMode)

	svc := &GinTracingService{Service: &TracingRequestService{Log: zap.NewNop()}}

	viper.Set("tracing.enabled", false)
	disabled := gin.New()
	require.NoError(t, svc.OnEngineInited(disabled))
	assert.Empty(t, disabled.Handlers, "关闭 tracing 时不应注册 Gin 中间件")

	viper.Set("tracing.enabled", true)
	enabled := gin.New()
	require.NoError(t, svc.OnEngineInited(enabled))
	assert.Len(t, enabled.Handlers, 1, "开启 tracing 时应注册 Gin 中间件")
}

func TestOutboundTracingDisabled(t *testing.T) {
	resetTracingConfig(t)

	rec := &recorderAdaptor{}
	old := TracingAdaptor
	TracingAdaptor = rec
	t.Cleanup(func() { TracingAdaptor = old })

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    200,
			ContentLength: 2,
			Body:          io.NopCloser(strings.NewReader("ok")),
			Header:        make(http.Header),
			Request:       req,
		}, nil
	})

	do := func() *http.Response {
		rt := LogTracying(TracingDetails{Optionname: "test.outbound"}, transport)
		req, err := http.NewRequest(http.MethodPost, "http://example.com/api", strings.NewReader("hello"))
		require.NoError(t, err)
		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)
		return resp
	}

	// 关闭：真实请求照常执行，但不推送 tracing，也不采集 body/resp。
	viper.Set("tracing.enabled", false)
	resp := do()
	require.NotNil(t, resp)
	resp.Body.Close()
	assert.Empty(t, rec.pushes)

	// 开启：正常采集并推送一条 tracing。
	viper.Set("tracing.enabled", true)
	resp = do()
	require.NotNil(t, resp)
	resp.Body.Close()
	require.Len(t, rec.pushes, 1)
	assert.Equal(t, "test.outbound", rec.pushes[0].Optionname)
	assert.Equal(t, "hello", string(rec.pushes[0].Body))
	assert.Equal(t, 200, rec.pushes[0].Status)
}

// recorderErrorAdaptor 记录错误上报，不启动投递 goroutine。
type recorderErrorAdaptor struct {
	pushes []core.ErrorReport
}

func (r *recorderErrorAdaptor) Push(data core.ErrorReport) { r.pushes = append(r.pushes, data) }
func (r *recorderErrorAdaptor) Sub(receiver string) <-chan core.ErrorReport {
	return nil
}
func (r *recorderErrorAdaptor) Subscripter(receiver string, fn core.Handler[core.ErrorReport]) {}
func (r *recorderErrorAdaptor) Start()                                                         {}
func (r *recorderErrorAdaptor) Stop()                                                          {}
func (r *recorderErrorAdaptor) Receivers() []string                                            { return nil }

var _ core.Adaptor[core.ErrorReport] = (*recorderErrorAdaptor)(nil)

// TestOutboundTransportErrorCarriesHappendATAndApp 锁定 2026-10-27 的根因修复：
// outbound 传输错误（timeout/dial/broken pipe）必须带 HappendAT 与 AppName，
// 否则落库变成 happened_at IS NULL / app_name 空，既不进时间范围查询，也不推进
// 「表最后写入时间」的新鲜度判据（当日因此误发 error 落库中断 P1）。
func TestOutboundTransportErrorCarriesHappendATAndApp(t *testing.T) {
	resetTracingConfig(t)

	rec := &recorderErrorAdaptor{}
	old := core.ErrorAdaptor
	core.ErrorAdaptor = rec
	t.Cleanup(func() { core.ErrorAdaptor = old })

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp 192.168.7.150:80: i/o timeout")
	})

	before := time.Now().Add(-time.Second)
	rt := LogTracying(TracingDetails{
		Optionname: "test.err",
		AppName:    "scm-order",
		AppVersion: "v1.2.3",
	}, transport)
	req, err := http.NewRequest(http.MethodPost,
		"http://192.168.7.150/GarmentLogistics/Garment/DETRFID/saveRFIDInfo", strings.NewReader("{}"))
	require.NoError(t, err)

	_, err = rt.RoundTrip(req)
	require.Error(t, err)

	require.Len(t, rec.pushes, 1)
	p := rec.pushes[0]
	assert.False(t, p.HappendAT.IsZero(), "传输错误必须带 HappendAT（否则落库为 NULL happened_at）")
	assert.True(t, p.HappendAT.After(before), "HappendAT 应取上报时刻")
	assert.Equal(t, "scm-order", p.AppName, "AppName 应取自 TracingDetails")
	assert.Equal(t, "v1.2.3", p.AppVersion)
	assert.Equal(t, "http://192.168.7.150/GarmentLogistics/Garment/DETRFID/saveRFIDInfo", p.Uri)
}
