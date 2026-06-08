package db

import (
	"time"

	"github.com/samber/lo"
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
	// orm.AppendEntity(&FullRequestDetails{})
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

func (tr *TracingRequestServiceDBImpl) startBatchWriter(ch chan monitor.TracingDetails) {
	batchSize := 100
	if viper.IsSet("tracing.db.batch.size") {
		batchSize = viper.GetInt("tracing.db.batch.size")
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	flushInterval := 10 * time.Second
	if viper.IsSet("tracing.db.batch.flushInterval") {
		if d := viper.GetDuration("tracing.db.batch.flushInterval"); d > 0 {
			flushInterval = d
		}
	} else if viper.IsSet("tracing.db.batch.flushIntervalSec") {
		if sec := viper.GetInt("tracing.db.batch.flushIntervalSec"); sec > 0 {
			flushInterval = time.Duration(sec) * time.Second
		}
	}

	queueSize := 10000
	if viper.IsSet("tracing.db.batch.queueSize") {
		queueSize = viper.GetInt("tracing.db.batch.queueSize")
	} else if batchSize > 0 {
		queueSize = batchSize * 100
		if queueSize < 10000 {
			queueSize = 10000
		}
	}
	if queueSize <= 0 {
		queueSize = 10000
	}

	queue := make(chan monitor.TracingDetails, queueSize)

	tr.Logger.Info("db tracing batch writer enabled",
		zap.Int("batchSize", batchSize),
		zap.Duration("flushInterval", flushInterval),
		zap.Int("queueSize", queueSize),
	)

	go func() {
		for v := range ch {
			queue <- v
		}
		close(queue)
	}()

	go func() {
		for {
			items, length, _, ok := lo.BufferWithTimeout(queue, batchSize, flushInterval)
			if length == 0 && ok {
				continue
			}

			models := make([]FullRequestDetails, 0, length)
			for _, item := range items {
				model, keep := tr.buildRequestModel(item)
				if !keep {
					continue
				}
				models = append(models, *model)
			}

			if len(models) > 0 {
				err := tr.DB.CreateInBatches(models, batchSize).Error
				if err != nil {
					tr.Logger.Error("batch insert request details failed", zap.Error(err), zap.Int("count", len(models)))
				} else {
					tr.Logger.Info("batch insert request details done", zap.Int("count", len(models)))
				}
			}

			if !ok {
				return
			}
		}
	}()
}

// func (tr *TracingRequestServiceDBImpl) doLogRequestBody(req monitor.TracingDetails) error {
// 	model, keep := tr.buildRequestModel(req)
// 	if !keep {
// 		return nil
// 	}
// 	err := tr.DB.Create(model).Error
// 	if err != nil {
// 		tr.Logger.Error("create request failed", zap.Error(err))
// 		return err
// 	}
// 	tr.Logger.Info("create request details done.", zap.Uint("targetID", req.TargetID))
// 	return nil
// }

func EnableDBMonitor() {
	orm.AppendEntity(&FullRequestDetails{})
	core.Provide(NewTracingRequestService)
	core.ProvideStartup(func(dbm *TracingRequestServiceDBImpl) core.Startup {
		ch := monitor.TracingAdaptor.Sub("db")
		if ch != nil {
			dbm.startBatchWriter(ch)
		}
		return nil
	})
	enableDBCleanup()
}
