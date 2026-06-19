package loki

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
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
	URL                         string
	User                        string
	Password                    string
	Protocol                    string
	MaxBytes                    int
	Included                    []string
	Excluded                    []string
	IncludedIPs                 []string
	ExcludedIPs                 []string
	QueueSize                   int
	WritePauseMS                int
	StartupPauseMS              int
	StartupSlowStartSeconds     int
	RetryPauseMS                int
	MaxRetryPauseMS             int
	ShutdownFlushTimeoutSeconds int
}

// lokiPushItem 表示一条待写入 Loki 的缓存消息。
// labels: 本条日志的 Loki labels。
// line: 待写入的正文内容。
// source: 数据来源，用于定位是哪类日志触发了写入。
// enqueuedAt: 入队时间，用于观测排队耗时。
type lokiPushItem struct {
	labels     map[string]string
	line       string
	source     string
	enqueuedAt time.Time
}

// LokiSetting 管理 Loki 写入配置、缓存队列与后台消费协程。
// 该类型负责把业务线程的同步直推改为“先缓存、再限速写入”。
type LokiSetting struct {
	monitor.BaseFilter
	// monitor.AppSettings
	Config               *LokiConfig
	FixedHeaders         map[string]string
	Logger               *zap.Logger
	MaxBytes             int
	client               LokiClient
	queue                chan lokiPushItem
	workerDone           chan struct{}
	queueMu              sync.RWMutex
	queueClosed          bool
	writePause           time.Duration
	startupPause         time.Duration
	startupSlowWindow    time.Duration
	retryPause           time.Duration
	maxRetryPause        time.Duration
	shutdownFlushTimeout time.Duration
	startedAt            time.Time
}

// LokiClient 抽象出 REST/gRPC 两种 Loki 客户端。
// Push: 按 labels 与正文写入一条日志。
// Close: 释放底层连接资源。
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
// 返回值：true 表示可重试，false 表示应直接丢弃并记录错误。
func isRetryableLokiError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "temporarily unavailable") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof")
}

// cloneLabels 复制一份 labels，避免入队后被后续逻辑修改。
// labels: 原始 label 集合。
// 返回值：新的 label 副本。
func cloneLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// InitLokiMonitor 初始化 Loki 配置、客户端与后台缓冲写入器。
// logger: 应用日志实例。
// 返回值：返回初始化完成的 LokiSetting；若未配置 URL，则返回 nil。
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
	queueSize := conf.QueueSize
	if queueSize <= 0 {
		queueSize = 20000
	}
	loki.queue = make(chan lokiPushItem, queueSize)
	loki.workerDone = make(chan struct{})
	loki.writePause = pickDurationByMillis(conf.WritePauseMS, 80*time.Millisecond)
	loki.startupPause = pickDurationByMillis(conf.StartupPauseMS, 250*time.Millisecond)
	loki.startupSlowWindow = pickDurationBySeconds(conf.StartupSlowStartSeconds, 120*time.Second)
	loki.retryPause = pickDurationByMillis(conf.RetryPauseMS, 1500*time.Millisecond)
	loki.maxRetryPause = pickDurationByMillis(conf.MaxRetryPauseMS, 15*time.Second)
	loki.shutdownFlushTimeout = pickDurationBySeconds(conf.ShutdownFlushTimeoutSeconds, 4*time.Second)
	loki.startedAt = time.Now()
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
		zap.Int("queueSize", queueSize),
		zap.Duration("writePause", loki.writePause),
		zap.Duration("startupPause", loki.startupPause),
		zap.Duration("startupSlowWindow", loki.startupSlowWindow),
		zap.Duration("retryPause", loki.retryPause),
		zap.Duration("maxRetryPause", loki.maxRetryPause),
		zap.Duration("shutdownFlushTimeout", loki.shutdownFlushTimeout),
	)

	core.OnServiceStopping(func() {
		loki.shutdownWriter()
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

	go loki.runWriter()
	logger.Info("Loki monitor service is ready.")
	return loki, nil
}

// ReportScheduleJob 将任务日志写入本地缓存队列。
// req: 定时任务执行记录。
// 返回值：仅在序列化失败时返回错误；入队异常由内部日志处理，不影响主流程。
func (lm *LokiSetting) ReportScheduleJob(req schedule.JobHistory) error {
	header := lm.cloneFixedHeader()
	setLokiLabel(header, "data_type", "cron_job")
	setLokiLabel(header, "app", req.App)
	setLokiLabel(header, "succeed", strconv.FormatBool(req.Succeed))
	setLokiLabel(header, "job", req.Job)

	body, _ := json.Marshal(req)
	return lm.enqueueLog("schedule", header, string(body))
}

// ReportError 将错误日志写入本地缓存队列。
// rr: 错误上报对象。
// 返回值：仅在正文编码失败等不可恢复场景下返回错误。
func (lm *LokiSetting) ReportError(rr core.ErrorReport) error {
	header := lm.cloneFixedHeader()
	setLokiLabel(header, "data_type", "error")
	setLokiLabel(header, "app", rr.AppName)
	setLokiLabel(header, "version", rr.AppVersion)

	bodyText, bodyEnc := monitor.EncodePayloadForText(rr.FullStack)
	setLokiLabel(header, "stack_enc", bodyEnc)
	return lm.enqueueLog("error", header, bodyText)
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

// ReportTracing 将 tracing 详情写入本地缓存队列。
// tr: tracing 详情。
// 返回值：仅在 JSON 序列化失败时返回错误。
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

	return lm.enqueueLog("tracing", header, string(body))
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

// enqueueLog 将日志正文先写入本地缓存队列，避免业务线程直接阻塞在 Loki。
// source: 数据来源类型，如 tracing/error/schedule。
// labels: Loki labels。
// line: 待写入的正文。
// 返回值：队列写入失败时也返回 nil，仅通过内部日志告警，避免监控链路反向影响业务。
func (lm *LokiSetting) enqueueLog(source string, labels map[string]string, line string) error {
	item := lokiPushItem{
		labels:     cloneLabels(labels),
		line:       line,
		source:     source,
		enqueuedAt: time.Now(),
	}

	lm.queueMu.RLock()
	defer lm.queueMu.RUnlock()
	if lm.queueClosed {
		lm.Logger.Warn("[loki-buffer] writer is stopping, skip enqueue",
			zap.String("source", source),
			zap.Int("pending", len(lm.queue)),
		)
		return nil
	}

	select {
	case lm.queue <- item:
		pending := len(lm.queue)
		capacity := cap(lm.queue)
		if capacity > 0 && pending*100 >= capacity*80 && pending%1000 == 0 {
			lm.Logger.Warn("[loki-buffer] queue is under pressure",
				zap.Int("pending", pending),
				zap.Int("capacity", capacity),
				zap.String("source", source),
			)
		}
		return nil
	default:
		lm.Logger.Warn("[loki-buffer] queue is full, drop message",
			zap.Int("capacity", cap(lm.queue)),
			zap.String("source", source),
			zap.String("labels", fmt.Sprintf("%v", labels)),
		)
		return nil
	}
}

// runWriter 持续消费缓存队列，并按限速策略写入 Loki。
// 返回值：无。
func (lm *LokiSetting) runWriter() {
	defer close(lm.workerDone)
	lm.Logger.Info("[loki-buffer] writer started",
		zap.Int("capacity", cap(lm.queue)),
		zap.Duration("writePause", lm.writePause),
		zap.Duration("startupPause", lm.startupPause),
	)
	for item := range lm.queue {
		lm.flushBufferedItem(item)
	}
	lm.Logger.Info("[loki-buffer] writer stopped")
}

// flushBufferedItem 将单条缓存消息拆分、重试并最终写入 Loki。
// item: 待落库的缓存消息。
// 返回值：无，内部负责记录完整日志。
func (lm *LokiSetting) flushBufferedItem(item lokiPushItem) {
	parts := splitWithPrefix(item.line, lm.MaxBytes)
	for idx, part := range parts {
		if !lm.pushWithRetry(item, part, idx+1, len(parts)) {
			return
		}
		lm.sleepBetweenWrites()
	}

	queueDelay := time.Since(item.enqueuedAt)
	if queueDelay >= 3*time.Second {
		lm.Logger.Info("[loki-buffer] buffered message flushed",
			zap.String("source", item.source),
			zap.Duration("queueDelay", queueDelay),
			zap.Int("pending", len(lm.queue)),
		)
	}
}

// pushWithRetry 写入单个分片，并在 429/超时等可恢复错误上执行退避重试。
// item: 当前处理的缓存消息。
// line: 当前分片正文。
// partIndex: 当前分片序号，从 1 开始。
// totalParts: 当前消息的总分片数。
// 返回值：true 表示可以继续处理；false 表示停机阶段应尽快结束当前消息。
func (lm *LokiSetting) pushWithRetry(item lokiPushItem, line string, partIndex int, totalParts int) bool {
	backoff := lm.retryPause
	for attempt := 1; ; attempt++ {
		err := lm.client.Push(item.labels, line)
		if err == nil {
			if attempt > 1 {
				lm.Logger.Info("[loki-buffer] push recovered",
					zap.String("source", item.source),
					zap.Int("attempt", attempt),
					zap.Int("partIndex", partIndex),
					zap.Int("totalParts", totalParts),
				)
			}
			return true
		}

		if !isRetryableLokiError(err) {
			lm.Logger.Error("[loki-buffer] non-retryable push failed, drop message",
				zap.String("source", item.source),
				zap.Int("partIndex", partIndex),
				zap.Int("totalParts", totalParts),
				zap.Error(err),
			)
			return true
		}

		if lm.isQueueClosed() && attempt >= 2 {
			lm.Logger.Warn("[loki-buffer] stop retry because service is shutting down",
				zap.String("source", item.source),
				zap.Int("attempt", attempt),
				zap.Error(err),
			)
			return false
		}

		if backoff > lm.maxRetryPause {
			backoff = lm.maxRetryPause
		}
		lm.Logger.Warn("[loki-buffer] retry push after backoff",
			zap.String("source", item.source),
			zap.Int("attempt", attempt),
			zap.Duration("backoff", backoff),
			zap.Int("pending", len(lm.queue)),
			zap.Error(err),
		)
		time.Sleep(backoff)
		if backoff < lm.maxRetryPause {
			backoff *= 2
		}
	}
}

// sleepBetweenWrites 在每次实际写入 Loki 后主动暂停，平滑重启后的突发流量。
// 返回值：无。
func (lm *LokiSetting) sleepBetweenWrites() {
	if lm.isQueueClosed() {
		return
	}
	pause := lm.writePause
	if time.Since(lm.startedAt) <= lm.startupSlowWindow && lm.startupPause > pause {
		pause = lm.startupPause
	}
	if pause <= 0 {
		return
	}
	time.Sleep(pause)
}

// isQueueClosed 判断 writer 是否已经进入停机排空阶段。
// 返回值：true 表示队列已关闭。
func (lm *LokiSetting) isQueueClosed() bool {
	lm.queueMu.RLock()
	defer lm.queueMu.RUnlock()
	return lm.queueClosed
}

// closeQueue 关闭入队通道，让后台 worker 进入“只排空不接收新数据”状态。
// 返回值：无。
func (lm *LokiSetting) closeQueue() {
	lm.queueMu.Lock()
	defer lm.queueMu.Unlock()
	if lm.queueClosed {
		return
	}
	lm.queueClosed = true
	close(lm.queue)
}

// shutdownWriter 停机时优先排空缓存，再关闭底层 Loki 客户端。
// 返回值：无。
func (lm *LokiSetting) shutdownWriter() {
	lm.Logger.Info("[loki-buffer] stopping writer",
		zap.Int("pending", len(lm.queue)),
		zap.Duration("flushTimeout", lm.shutdownFlushTimeout),
	)
	lm.closeQueue()

	timer := time.NewTimer(lm.shutdownFlushTimeout)
	defer timer.Stop()
	select {
	case <-lm.workerDone:
		lm.Logger.Info("[loki-buffer] pending messages flushed",
			zap.Int("pending", len(lm.queue)),
		)
	case <-timer.C:
		lm.Logger.Warn("[loki-buffer] flush timeout reached, stop waiting",
			zap.Int("pending", len(lm.queue)),
		)
	}

	if err := lm.client.Close(); err != nil {
		lm.Logger.Warn("[loki-buffer] close client failed", zap.Error(err))
	}
}

// pushSplit 兼容旧调用路径，当前已切换为“先入队、后异步写入”的实现。
// labels: Loki labels。
// line: 待写入正文。
// 返回值：当前实现通常返回 nil。
func (lm *LokiSetting) pushSplit(labels map[string]string, line string) error {
	return lm.enqueueLog("legacy", labels, line)
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
