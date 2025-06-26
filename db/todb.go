package db

import (
	"time"

	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/orm"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type FullRequestDetails struct {
	gorm.Model
	Optionname string `gorm:"size:64"`
	Operator   string `gorm:"size:64"`
	Uri        string `gorm:"size:256"`
	Method     string `gorm:"size:16"`
	Body       []byte
	Durtion    time.Duration
	Status     int
	TargetID   uint
	Resp       []byte
	ClientIP   string `gorm:"size:64"`
	UserAgent  string `gorm:"size:256"`
	Device     string `gorm:"size:64"`
}

type TracingRequestServiceDBImpl struct {
	DB     *gorm.DB
	Logger *zap.Logger
}

func NewTracingRequestService(db *gorm.DB, logger *zap.Logger) (*TracingRequestServiceDBImpl, error) {
	tr := &TracingRequestServiceDBImpl{
		DB:     db,
		Logger: logger,
	}
	// orm.AppendEntity(&FullRequestDetails{})
	return tr, nil
}

// func SubEventToDB(tr *TracingRequestServiceDBImpl, bus EventBus.Bus) {
// 	// bus.SubscribeAsync(core.EventTracing, tr.doLogRequestBody, false)
// 	monitor.TracingAdaptor.Subscripter("db", tr.doLogRequestBody)
// }

func (tr *TracingRequestServiceDBImpl) doLogRequestBody(req monitor.TracingDetails) error {
	if req.Body == "" && req.Resp == "" {
		tr.Logger.Debug("both req & resp is emtpy, ignored.")
		return nil
	}

	// if bts, ok := req.Body.([]byte); ok {
	// 	if resp, ok := req.Resp.([]byte); ok {
	// 		if len(bts) == 0 && len(resp) == 0 {
	// 			tr.Logger.Debug("request body is empty, ignored.")
	// 			return nil
	// 		}
	// 	}
	// }

	// if body, ok := req.Body.(string); ok {
	// 	if body == "" {
	// 		if resp, ok := req.Resp.(string); ok {
	// 			if resp == "" {
	// 				tr.Logger.Debug("request body is empty, ignored.")
	// 				return nil
	// 			}
	// 		}
	// 	}
	// }

	model := FullRequestDetails{
		Optionname: req.Optionname,
		Operator:   req.Operator,
		Uri:        req.Uri,
		Method:     req.Method,
		Body:       []byte(req.Body),
		Durtion:    req.Durtion,
		Status:     req.Status,
		TargetID:   req.TargetID,
		Resp:       []byte(req.Resp),
		ClientIP:   req.ClientIP,
		UserAgent:  req.UserAgent,
		Device:     req.Device,
	}

	err := tr.DB.Save(&model).Error
	if err != nil {
		tr.Logger.Error("save reqest failed", zap.Error(err))
		return err
	}
	tr.Logger.Info("save request details done.", zap.Uint("targetID", req.TargetID))
	return nil
}

func EnableDBMonitor() {
	orm.AppendEntity(&FullRequestDetails{})
	core.Provide(NewTracingRequestService)
	core.ProvideStartup(func(dbm *TracingRequestServiceDBImpl) core.Startup {
		monitor.TracingAdaptor.Subscripter("db", dbm.doLogRequestBody)
		return nil
	})
}
