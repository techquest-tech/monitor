package db

import (
	"time"

	"github.com/asaskevich/EventBus"
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
	orm.AppendEntity(&FullRequestDetails{})
	return tr, nil
}

func SubEventToDB(tr *TracingRequestServiceDBImpl, bus EventBus.Bus) {
	bus.SubscribeAsync(core.EventTracing, tr.doLogRequestBody, false)
}

func (tr *TracingRequestServiceDBImpl) doLogRequestBody(req *monitor.TracingDetails) {
	if req.Body == nil && req.Resp == nil {
		tr.Logger.Debug("both req & resp is emtpy, ignored.")
		return
	}

	if bts, ok := req.Body.([]byte); ok {
		if resp, ok := req.Resp.([]byte); ok {
			if len(bts) == 0 && len(resp) == 0 {
				tr.Logger.Debug("request body is empty, ignored.")
				return
			}
		}
	}

	if body, ok := req.Body.(string); ok {
		if body == "" {
			if resp, ok := req.Resp.(string); ok {
				if resp == "" {
					tr.Logger.Debug("request body is empty, ignored.")
					return
				}
			}
		}
	}

	model := FullRequestDetails{
		Optionname: req.Optionname,
		Operator:   req.Operator,
		Uri:        req.Uri,
		Method:     req.Method,
		Body:       monitor.ToByte(req.Body),
		Durtion:    req.Durtion,
		Status:     req.Status,
		TargetID:   req.TargetID,
		Resp:       monitor.ToByte(req.Resp),
		ClientIP:   req.ClientIP,
		UserAgent:  req.UserAgent,
		Device:     req.Device,
	}

	err := tr.DB.Save(&model).Error
	if err != nil {
		tr.Logger.Error("save reqest failed", zap.Error(err))
		return
	}
	tr.Logger.Info("save request details done.", zap.Uint("targetID", req.TargetID))
}

func EnableDBMonitor() {
	// orm.AppendEntity(&FullRequestDetails{})
	core.Provide(NewTracingRequestService)
	core.ProvideStartup(func(dbm *TracingRequestServiceDBImpl, bus EventBus.Bus) core.Startup {
		SubEventToDB(dbm, bus)
		return nil
	})
}
