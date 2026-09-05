package loki

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

type LokiConfig struct {
	URL                     string
	User                    string
	Password                string
	Protocol                string
	MaxBytes                int
	Included                []string
	Excluded                []string
	IncludedIPs             []string
	ExcludedIPs             []string
	BatchMaxBytes           int
	WritePauseMS            int
	StartupPauseMS          int
	StartupSlowStartSeconds int
	RetryPauseMS            int
	MaxRetryPauseMS         int
}

// maxPushAttempts 单批推送的最大尝试次数：超过后返回 error，令 Redis 侧整批
// 保持 pending 由 pending-check 重投（指数退避覆盖瞬态抖动，长期故障交给
// Redis 重投 + 死信）。失败即不 ACK —— ACK 已后移到「真正 push 到 Loki 成功」之后。
const maxPushAttempts = 6

// PushEntry 是批量写入中的单条日志（含已拆分的分片）。
type PushEntry struct {
	Labels map[string]string
	Line   string
	Ts     time.Time
}

// LokiSetting 管理 Loki 写入：每个批量 handler 直接同步推送到 Loki（含指数退避
// 重试与自适应限速），成功才返回 nil。不再有进程内异步缓冲队列——ACK 由 Redis
// 消费层在 handler 返回 nil 后发出，因此进程崩溃时不会丢失「已入队未推送」的数据。
type LokiSetting struct {
	monitor.BaseFilter
	Config        *LokiConfig
	FixedHeaders  map[string]string
	Logger        *zap.Logger
	MaxBytes      int
	client        LokiClient
	batchMaxBytes int

	// 自适应写间隔（限速）参数
	writePause        time.Duration
	startupPause      time.Duration
	startupSlowWindow time.Duration
	retryPause        time.Duration
	maxRetryPause     time.Duration
	startedAt         time.Time
	currentPause      time.Duration
	minPause          time.Duration

	// mu 串行化推送到 Loki 及 currentPause 的读写（tracing/schedule/error
	// 三个批量消费者可能并发调用）；closed 标记停机，pushBatch 据此停止重试。
	mu     sync.Mutex
	closed atomic.Bool
}

// LokiClient 抽象出 REST/gRPC 两种 Loki 客户端。
// PushBatch: 批量写入一组日志（同 label 的日志聚合到同一 stream）。
// Close: 释放底层连接资源。
type LokiClient interface {
	PushBatch(entries []PushEntry) error
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

// pickDurationByMillis 读取毫秒级配置，没有配置时回退到默认值。
// valueMS: 配置中的毫秒值。
// fallback: 默认值。
// 返回值：最终采用的时长。
func pickDurationByMillis(valueMS int, fallback time.Duration) time.Duration {
	if valueMS <= 0 {
		return fallback
	}
	return time.Duration(valueMS) * time.Millisecond
}

// pickDurationBySeconds 读取秒级配置，没有配置时回退到默认值。
// valueSeconds: 配置中的秒数。
// fallback: 默认值。
// 返回值：最终采用的时长。
func pickDurationBySeconds(valueSeconds int, fallback time.Duration) time.Duration {
	if valueSeconds <= 0 {
		return fallback
	}
	return time.Duration(valueSeconds) * time.Second
}

// isRetryableLokiError 判断错误是否适合做退避重试。
// err: Loki 推送返回的错误。
// 返回值：true 表示可重试，false 表示应直接返回错误（保留 pending 待重投）。
//
// 注意：REST/gRPC 客户端的超时错误是 "context deadline exceeded"（不含
// "timeout" 子串），必须单独匹配，否则超时会被误判为不可重试。
func isRetryableLokiError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "temporarily unavailable") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "status=5") // 500/502/503/504 等瞬时服务端错误可重试
}

// InitLokiMonitor 初始化 Loki 配置、客户端与自适应限速状态。
// logger: 应用日志实例。
// 返回值：返回初始化完成的 LokiSetting；若未配置 URL，则返回 nil。
func InitLokiMonitor(logger *zap.Logger) (*LokiSetting, error) {
	loki := &LokiSetting{
		FixedHeaders: map[string]string{},
		Logger:       logger,
		MaxBytes:     240 * 1024,
	}
	conf := &LokiConfig{}
	err := viper.UnmarshalKey("tracing.loki", conf)
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
	loki.batchMaxBytes = conf.BatchMaxBytes
	if loki.batchMaxBytes <= 0 {
		loki.batchMaxBytes = 1024 * 1024
	}
	loki.writePause = pickDurationByMillis(conf.WritePauseMS, 80*time.Millisecond)
	loki.startupPause = pickDurationByMillis(conf.StartupPauseMS, 250*time.Millisecond)
	loki.startupSlowWindow = pickDurationBySeconds(conf.StartupSlowStartSeconds, 120*time.Second)
	loki.retryPause = pickDurationByMillis(conf.RetryPauseMS, 1500*time.Millisecond)
	loki.maxRetryPause = pickDurationByMillis(conf.MaxRetryPauseMS, 15*time.Second)
	loki.startedAt = time.Now()
	loki.currentPause = loki.writePause
	loki.minPause = 20 * time.Millisecond
	loki.BaseFilter = monitor.BaseFilter{
		Included:    conf.Included,
		Excluded:    conf.Excluded,
		IncludedIPs: conf.IncludedIPs,
		ExcludedIPs: conf.ExcludedIPs,
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
		zap.Int("maxBytes", loki.MaxBytes),
		zap.Int("batchMaxBytes", loki.batchMaxBytes),
		zap.Duration("writePause", loki.writePause),
		zap.Duration("startupPause", loki.startupPause),
		zap.Duration("startupSlowWindow", loki.startupSlowWindow),
		zap.Duration("retryPause", loki.retryPause),
		zap.Duration("maxRetryPause", loki.maxRetryPause),
	)

	core.OnServiceStopping(func() {
		loki.close()
	})

	// app/version 不在此统一写死为基础 label：各批量写入按单条记录自身的
	// AppName/AppVersion 打 label，仅当记录缺失时才用 core.AppName/core.Version
	// 兜底（见 ReportTracingBatch / ReportErrorBatch / ReportScheduleJobBatch）。
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

// close 标记停机并关闭底层客户端；正在重试的推送会在下一次尝试前看到 closed
// 并返回 error，令 Redis 侧整批保持 pending。
func (lm *LokiSetting) close() {
	lm.closed.Store(true)
	if lm.client != nil {
		_ = lm.client.Close()
	}
}

// cloneFixedHeader 复制基础 labels，避免多个并发请求共用同一份 map。
// 返回值：新的 labels 副本。
func (lm *LokiSetting) cloneFixedHeader() map[string]string {
	out := map[string]string{}
	for k, v := range lm.FixedHeaders {
		out[k] = v
	}
	return out
}

// --- MonitorService 单条方法（批量模式下委托批量实现） ---

func (lm *LokiSetting) ReportTracing(tr monitor.TracingDetails) error {
	return lm.ReportTracingBatch([]monitor.TracingDetails{tr})
}

func (lm *LokiSetting) ReportError(rr core.ErrorReport) error {
	return lm.ReportErrorBatch([]core.ErrorReport{rr})
}

func (lm *LokiSetting) ReportScheduleJob(req schedule.JobHistory) error {
	return lm.ReportScheduleJobBatch([]schedule.JobHistory{req})
}

// --- MonitorBatchService 批量方法：组装 entries 后同步推送 ---

// ReportTracingBatch 批量上报 tracing：组装 label + 正文（超长按 MaxBytes 拆分）
// 后同步推送到 Loki；推送失败返回 error（不 ACK），由 Redis pending-check 重投。
func (lm *LokiSetting) ReportTracingBatch(trs []monitor.TracingDetails) error {
	if len(trs) == 0 {
		return nil
	}
	entries := make([]PushEntry, 0, len(trs))
	for _, tr := range trs {
		header := lm.cloneFixedHeader()
		setLokiLabel(header, "data_type", "tracing")
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
		now := time.Now()
		for _, part := range splitWithPrefix(string(body), lm.MaxBytes) {
			entries = append(entries, PushEntry{Labels: header, Line: part, Ts: now})
		}
	}
	return lm.pushEntries(entries)
}

// ReportErrorBatch 批量上报错误：错误正文取 FullStack，超长拆分后同步推送。
func (lm *LokiSetting) ReportErrorBatch(rrs []core.ErrorReport) error {
	if len(rrs) == 0 {
		return nil
	}
	entries := make([]PushEntry, 0, len(rrs))
	for _, rr := range rrs {
		header := lm.cloneFixedHeader()
		setLokiLabel(header, "data_type", "error")
		app := rr.AppName
		if app == "" {
			app = core.AppName
		}
		version := rr.AppVersion
		if version == "" {
			version = core.Version
		}
		setLokiLabel(header, "app", app)
		setLokiLabel(header, "version", version)

		bodyText, bodyEnc := monitor.EncodePayloadForText(rr.FullStack)
		setLokiLabel(header, "stack_enc", bodyEnc)
		now := time.Now()
		for _, part := range splitWithPrefix(bodyText, lm.MaxBytes) {
			entries = append(entries, PushEntry{Labels: header, Line: part, Ts: now})
		}
	}
	return lm.pushEntries(entries)
}

// ReportScheduleJobBatch 批量上报定时任务历史，同步推送。
func (lm *LokiSetting) ReportScheduleJobBatch(reqs []schedule.JobHistory) error {
	if len(reqs) == 0 {
		return nil
	}
	entries := make([]PushEntry, 0, len(reqs))
	for _, req := range reqs {
		header := lm.cloneFixedHeader()
		setLokiLabel(header, "data_type", "cron_job")
		app := req.App
		if app == "" {
			app = core.AppName
		}
		version := req.AppVersion
		if version == "" {
			version = core.Version
		}
		setLokiLabel(header, "app", app)
		setLokiLabel(header, "version", version)
		setLokiLabel(header, "succeed", strconv.FormatBool(req.Succeed))
		setLokiLabel(header, "job", req.Job)

		body, _ := json.Marshal(req)
		now := time.Now()
		for _, part := range splitWithPrefix(string(body), lm.MaxBytes) {
			entries = append(entries, PushEntry{Labels: header, Line: part, Ts: now})
		}
	}
	return lm.pushEntries(entries)
}

// splitUTF8ByBytes 按字节数拆分字符串，并尽量保证 UTF-8 边界完整。
// s: 原始字符串。
// max: 每段允许的最大字节数。
// 返回值：拆分后的字符串切片。
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

// splitWithPrefix 在拆分后的每段前追加 part 前缀，便于 Loki 中复原大报文。
// s: 原始字符串。
// max: 每段允许的最大字节数。
// 返回值：带分片前缀的字符串切片。
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

// pushEntries 串行化地按 batchMaxBytes 切块推送 entries，全部成功后按自适应
// 间隔限速。返回 error 表示推送失败（未 ACK）。
func (lm *LokiSetting) pushEntries(entries []PushEntry) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	for len(entries) > 0 {
		n := 0
		bytes := 0
		for n < len(entries) && bytes+len(entries[n].Line) <= lm.batchMaxBytes {
			bytes += len(entries[n].Line)
			n++
		}
		if n == 0 {
			n = 1 // 单条分片本身就超过 batchMaxBytes，只能单独推送
		}
		if err := lm.pushBatch(entries[:n]); err != nil {
			return err
		}
		entries = entries[n:]
	}

	lm.adaptivePause()
	return nil
}

// pushBatch 将一批分片写入 Loki，在 429/超时/5xx 等可恢复错误上退避重试（最多
// maxPushAttempts 次）；不可恢复或重试耗尽则返回 error。成功后根据是否发生过
// 重试调整自适应写间隔。
func (lm *LokiSetting) pushBatch(entries []PushEntry) error {
	backoff := lm.retryPause
	recovered := false
	var lastErr error
	for attempt := 1; attempt <= maxPushAttempts; attempt++ {
		if lm.closed.Load() {
			return errors.New("loki writer is stopping")
		}
		err := lm.client.PushBatch(entries)
		if err == nil {
			if attempt > 1 {
				recovered = true
				lm.Logger.Info("[loki] push recovered",
					zap.Int("entries", len(entries)),
					zap.Int("attempt", attempt),
				)
			}
			lastErr = nil
			break
		}

		lastErr = err
		if !isRetryableLokiError(err) {
			lm.Logger.Error("[loki] non-retryable push failed",
				zap.Int("entries", len(entries)),
				zap.Error(err),
			)
			break
		}

		recovered = true
		if attempt == maxPushAttempts {
			lm.Logger.Error("[loki] push retries exhausted, keep pending for redelivery",
				zap.Int("entries", len(entries)),
				zap.Int("attempt", attempt),
				zap.Error(err),
			)
			break
		}
		if backoff > lm.maxRetryPause {
			backoff = lm.maxRetryPause
		}
		lm.Logger.Warn("[loki] retry push after backoff",
			zap.Int("entries", len(entries)),
			zap.Int("attempt", attempt),
			zap.Duration("backoff", backoff),
			zap.Error(err),
		)
		time.Sleep(backoff)
		if backoff < lm.maxRetryPause {
			backoff *= 2
		}
	}

	// 自适应限速：发生重试（429/5xx/超时）则倍增间隔，干净成功则减半，向
	// 下不越过 minPause、向上不越过 maxRetryPause。
	if recovered {
		if lm.currentPause < lm.maxRetryPause {
			lm.currentPause *= 2
			if lm.currentPause > lm.maxRetryPause {
				lm.currentPause = lm.maxRetryPause
			}
		}
	} else if lm.currentPause > lm.minPause {
		lm.currentPause /= 2
		if lm.currentPause < lm.minPause {
			lm.currentPause = lm.minPause
		}
	}

	return lastErr
}

// adaptivePause 在每批推送后按自适应间隔暂停；启动后的慢启动窗口内使用更
// 大的 startupPause 平滑突发流量。调用方（pushEntries）已持 mu。
func (lm *LokiSetting) adaptivePause() {
	if lm.closed.Load() {
		return
	}
	pause := lm.currentPause
	if time.Since(lm.startedAt) <= lm.startupSlowWindow && lm.startupPause > pause {
		pause = lm.startupPause
	}
	if pause <= 0 {
		return
	}
	time.Sleep(pause)
}

// EnableLokiMonitor 向容器注册 Loki 监控服务，并在启动时订阅监控事件。
// 返回值：无。
func EnableLokiMonitor() {
	core.Provide(InitLokiMonitor)
	core.ProvideStartup(func(logger *zap.Logger, s *LokiSetting) core.Startup {
		if s != nil {
			monitor.SubscribeMonitor(logger, s)
		}

		return nil
	})
}
