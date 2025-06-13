package messaging

import (
	"context"
	"encoding/json"

	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/messaging"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

var (
	tracing = &RedisAdaptor[monitor.TracingDetails]{
		Topic:        "monitor.tracing",
		ChanAdaaptor: monitor.TracingAdaptor,
	}

	jobReport = &RedisAdaptor[schedule.JobHistory]{
		Topic:        "monitor.schedule",
		ChanAdaaptor: schedule.JobHistoryAdaptor,
	}
	errorReport = &RedisAdaptor[core.ErrorReport]{
		Topic:        "monitor.error",
		ChanAdaaptor: core.ErrorAdaptor,
	}
)

// redis monitor bridge, monitor data to redis first, then via redis adaptor to real monitor persist.
func EnabledRedisBridge() {
	core.ProvideStartup(func(logger *zap.Logger, service messaging.MessagingService) core.Startup {
		tracing.AsBridge(service)
		jobReport.AsBridge(service)
		errorReport.AsBridge(service)

		zap.L().Info("messaging service as bridge enabled")
		return nil
	})
}

func RunAsAdaptor() error {
	return core.GetContainer().Invoke(func(logger *zap.Logger, service messaging.MessagingService, _ core.Startups) {
		ctx := context.Background()
		consumer := "monitor-adaptor"

		service.Sub(ctx, tracing.Topic, consumer, tracing.Adaptor)
		service.Sub(ctx, jobReport.Topic, consumer, jobReport.Adaptor)
		service.Sub(ctx, errorReport.Topic, consumer, errorReport.Adaptor)
		zap.L().Info("messaging service as adaptor enabled")
	})
}

type RedisAdaptor[T any] struct {
	ChanAdaaptor *core.ChanAdaptor[T]
	Topic        string
}

func (r *RedisAdaptor[T]) AsBridge(service messaging.MessagingService) {
	r.ChanAdaaptor.Subscripter("redis.bridge", func(data T) error {
		return service.Pub(context.Background(), r.Topic, data)
	})
}
func (r *RedisAdaptor[T]) Adaptor(ctx context.Context, topic, consumer string, payload []byte) error {
	logger := zap.L()

	var tr T
	if err := json.Unmarshal(payload, &tr); err != nil {
		logger.Error("unexpected tracing details format", zap.ByteString("payload", payload), zap.Error(err))

		messaging.AbandonedChan <- map[string]any{
			"topic": topic,
			"raw":   payload,
			"error": err.Error(),
		}
		return nil
	}
	r.ChanAdaaptor.Push(tr)
	return nil
}
