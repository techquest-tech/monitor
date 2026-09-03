package db

import (
	"time"

	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/orm"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type FullRequestDetails struct {
	gorm.Model
	Optionname     string                        `gorm:"size:256"`
	AppName        string                        `gorm:"size:64"`
	AppVersion     string                        `gorm:"size:64"`
	Operator       string                        `gorm:"size:64"`
	Tenant         string                        `gorm:"size:64"`
	Uri            string                        `gorm:"size:256"`
	Method         string                        `gorm:"size:16"`
	VerbosityLevel monitor.TracingVerbosityLevel `gorm:"default:99"`
	Body           []byte
	BodyText       string `gorm:"type:longtext"`
	BodyEnc        string `gorm:"size:16"`
	Durtion        time.Duration
	Status         int
	TargetID       uint
	Resp           []byte
	RespText       string `gorm:"type:longtext"`
	RespEnc        string `gorm:"size:16"`
	ClientIP       string `gorm:"size:64"`
	UserAgent      string `gorm:"size:256"`
	Device         string `gorm:"size:64"`
}

type TracingRequestServiceDBImpl struct {
	DB     *gorm.DB
	Logger *zap.Logger
}

// ErrorReportDetails 错误上报落库模型，对应表 prd_error_report_details / uat_error_report_details
// （gorm 默认命名：error_text / full_stack / happend_at，与 README monitor_db 数据模型一致）。
type ErrorReportDetails struct {
	gorm.Model
	Uri       string    `gorm:"size:256"`
	FullStack []byte
	ErrorText string    `gorm:"size:2048"`
	HappendAT time.Time
}

// dbInsertChunk 单条 INSERT 语句的最大行数：批量到达的数据按此分块写入，
// 避免单条语句过大触发 MySQL max_allowed_packet 限制。
const dbInsertChunk = 100

func NewTracingRequestService(db *gorm.DB, logger *zap.Logger) (*TracingRequestServiceDBImpl, error) {
	gormLogLevel := "error"
	if viper.IsSet("tracing.db.gorm.logLevel") {
		gormLogLevel = viper.GetString("tracing.db.gorm.logLevel")
	}
	slowThreshold := 0 * time.Millisecond
	if viper.IsSet("tracing.db.gorm.slowThreshold") {
		slowThreshold = viper.GetDuration("tracing.db.gorm.slowThreshold")
	} else if viper.IsSet("tracing.db.gorm.slowThresholdMs") {
		slowThreshold = time.Duration(viper.GetInt("tracing.db.gorm.slowThresholdMs")) * time.Millisecond
	}

	tr := &TracingRequestServiceDBImpl{
		DB:     db.Session(&gorm.Session{Logger: orm.NewGormLogger(slowThreshold, gormLogLevel)}),
		Logger: logger,
	}
	return tr, nil
}

func (tr *TracingRequestServiceDBImpl) buildRequestModel(req monitor.TracingDetails) (*FullRequestDetails, bool) {
	if len(req.Body) == 0 && len(req.Resp) == 0 {
		tr.Logger.Debug("both req & resp is emtpy, ignored.")
		return nil, false
	}

	storeMax := monitor.TracingVerbosityLevelRead
	if viper.IsSet("tracing.db.storeMaxVerbosityLevel") {
		storeMax = monitor.TracingVerbosityLevel(viper.GetInt("tracing.db.storeMaxVerbosityLevel"))
	}
	if req.VerbosityLevel > storeMax {
		return nil, false
	}

	bodyText, bodyEnc := monitor.EncodePayloadForText(req.Body)
	respText, respEnc := monitor.EncodePayloadForText(req.Resp)

	model := &FullRequestDetails{
		Optionname:     req.Optionname,
		AppName:        req.AppName,
		AppVersion:     req.AppVersion,
		Operator:       req.Operator,
		Tenant:         req.Tenant,
		Uri:            req.Uri,
		Method:         req.Method,
		VerbosityLevel: req.VerbosityLevel,
		BodyText:       bodyText,
		BodyEnc:        bodyEnc,
		Durtion:        req.Durtion,
		Status:         req.Status,
		TargetID:       req.TargetID,
		RespText:       respText,
		RespEnc:        respEnc,
		ClientIP:       req.ClientIP,
		UserAgent:      req.UserAgent,
		Device:         req.Device,
	}
	return model, true
}

// ReportTracingBatch 批量落库请求追踪详情：整批直接写入，不再经过进程内缓冲层。
// 返回 error 时（Redis 路径）整批保持 pending 由 pending-check 重投，保留原有
// 重试机制，避免进程崩溃时缓冲内数据丢失。
func (tr *TracingRequestServiceDBImpl) ReportTracingBatch(reqs []monitor.TracingDetails) error {
	models := make([]FullRequestDetails, 0, len(reqs))
	for _, req := range reqs {
		model, keep := tr.buildRequestModel(req)
		if !keep {
			continue
		}
		models = append(models, *model)
	}
	if len(models) == 0 {
		return nil
	}
	if err := tr.DB.CreateInBatches(models, dbInsertChunk).Error; err != nil {
		tr.Logger.Error("batch insert request details failed", zap.Error(err), zap.Int("count", len(models)))
		return err
	}
	tr.Logger.Info("batch insert request details done", zap.Int("count", len(models)))
	return nil
}

func (tr *TracingRequestServiceDBImpl) buildErrorModel(rr core.ErrorReport) (*ErrorReportDetails, bool) {
	errText := ""
	if rr.Error != nil {
		errText = rr.Error.Error()
	}
	// 空错误（无文案、无堆栈、无 URI）无落库价值
	if errText == "" && len(rr.FullStack) == 0 && rr.Uri == "" {
		return nil, false
	}
	uri := truncateRunes(rr.Uri, 256)
	errText = truncateRunes(errText, 2048)
	happend := rr.HappendAT
	if happend.IsZero() {
		// 上游未上报时间（如 monitor.Log 链路 HappendAT 为零值），MySQL datetime(3) 不支持 0001-01-01，
		// 回退到当前时间保证可落库
		happend = time.Now()
	}
	return &ErrorReportDetails{
		Uri:       uri,
		FullStack: rr.FullStack,
		ErrorText: errText,
		HappendAT: happend,
	}, true
}

// truncateRunes 按字符数截断，避免超出 varchar 列上限导致整批插入失败。
func truncateRunes(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// ReportErrorBatch 批量落库错误上报：整批直接写入，无进程内缓冲。写失败时（Redis
// 路径）整批保持 pending 由 pending-check 重投。
func (tr *TracingRequestServiceDBImpl) ReportErrorBatch(reqs []core.ErrorReport) error {
	models := make([]ErrorReportDetails, 0, len(reqs))
	for _, rr := range reqs {
		model, keep := tr.buildErrorModel(rr)
		if !keep {
			continue
		}
		models = append(models, *model)
	}
	if len(models) == 0 {
		return nil
	}
	if err := tr.DB.CreateInBatches(models, dbInsertChunk).Error; err != nil {
		tr.Logger.Error("batch insert error report details failed", zap.Error(err), zap.Int("count", len(models)))
		return err
	}
	tr.Logger.Info("batch insert error report details done", zap.Int("count", len(models)))
	return nil
}

func EnableDBMonitor() {
	orm.AppendEntity(&FullRequestDetails{})
	core.Provide(NewTracingRequestService)
	core.ProvideStartup(func(dbm *TracingRequestServiceDBImpl) core.Startup {
		// 批量订阅：整批直接落库，无进程内缓冲层（queue + lo.BufferWithTimeout 已移除，
		// 避免进程崩溃时缓冲内数据丢失）；写失败时（Redis 路径）整批保持 pending 由
		// pending-check 重投，保留原有重试机制。
		monitor.SubscribeBatch(monitor.TracingAdaptor, "db", "tracing.batch", dbm.ReportTracingBatch)

		// 错误落库开关：默认关闭，避免影响所有启用 monitor_db 的消费方。
		// 仅当配置 tracing.db.error.enabled=true 时才注册错误实体并订阅 core.ErrorAdaptor，
		// 保证错误只在唯一一处（当前为 monitor-adaptor）统一写入。
		if viper.GetBool("tracing.db.error.enabled") {
			orm.AppendEntity(&ErrorReportDetails{})
			monitor.SubscribeBatch(core.ErrorAdaptor, "db", "error.batch", dbm.ReportErrorBatch)
		}
		return nil
	})
}
