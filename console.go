package monitor

import "go.uber.org/zap"

type ConsoleTracing struct {
	Log *zap.Logger
}

func (tr *ConsoleTracing) LogBody(req TracingDetails) error {
	log := tr.Log.With(
		zap.String("method", req.Method),
		zap.String("uri", req.Uri),
		zap.Int("verbosityLevel", int(req.VerbosityLevel)),
	)
	if len(req.Body) > 0 {
		bodyText, bodyEnc := EncodePayloadForText(req.Body)
		log.Debug("req", zap.String("req encoding", bodyEnc), zap.String("req body", bodyText))
	}
	if len(req.Resp) > 0 {
		respText, respEnc := EncodePayloadForText(req.Resp)
		log.Debug("resp",
			zap.Int("status code", req.Status),
			zap.String("resp encoding", respEnc),
			zap.String("resp", respText))
	}
	return nil
}

func InitConsoleTracingService(log *zap.Logger) *ConsoleTracing {
	log.Debug("console tracing is enabled")
	return &ConsoleTracing{
		Log: log,
	}
}
