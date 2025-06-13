package monitor

import (
	"encoding/json"

	"go.uber.org/zap"
)

type ConsoleTracing struct {
	Log *zap.Logger
}

func ToByte(obj any) []byte {
	if obj == nil {
		return []byte{}
	}
	if bt, ok := obj.([]byte); ok {
		return bt
	}
	if str, ok := obj.(string); ok {
		return []byte(str)
	}
	result, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return result
}

func (tr *ConsoleTracing) LogBody(req TracingDetails) error {
	log := tr.Log.With(zap.String("method", req.Method), zap.String("uri", req.Uri))
	if req.Body != "" {
		log.Debug("req", zap.ByteString("req body", ToByte(req.Body)))
	}
	if req.Resp != "" {
		log.Debug("resp", zap.Int("status code", req.Status), zap.ByteString("resp", ToByte(req.Resp)))
	}
	return nil
}

func InitConsoleTracingService(log *zap.Logger) *ConsoleTracing {
	log.Debug("console tracing is enabled")
	return &ConsoleTracing{
		Log: log,
	}
}
