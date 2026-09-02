package messaging

import (
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/messaging"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

// EnableRedisAdaptors 把全局监控 adaptor（tracing / schedule / error）直接换成
// Redis Streams 实现（RedisAdaptor），生产与消费都基于 core.Adaptor /
// core.BatchingAdaptor 接口操作，不再有 redis ↔ chan 的转换层：
//
//	生产者:  TracingAdaptor.Push(...) -> XADD 到 redis stream
//	消费者:  SubscripterBatch(...)   -> 消费组，整批写入成功才 XAck
//
// 旧的中间一跳
//
//	Redis stream -> MessagingAdaptor -> 进程内 chan -> 消费者
//
// 已移除：没有双重 JSON 转换，没有「Redis 已 ack、内存这跳又丢」的中间 chan
// 丢失窗口；消费端写失败整批保持 pending 由 pending-check 重投。
//
// 必须在消费者订阅之前调用（如 main 里 ginshared.Start() 之前）。
// 当 messaging 服务不是 redis 后端时（ram / no_messaging 构建），保持进程内
// chan adaptor 不变。
func EnableRedisAdaptors(service messaging.MessagingService) {
	logger := zap.L()
	redis, ok := service.(*messaging.DefaultMessgingService)
	if !ok {
		logger.Warn("messaging service is not redis-backed, keep in-process chan adaptors")
		return
	}

	monitor.TracingAdaptor = messaging.NewRedisAdaptor[monitor.TracingDetails]("monitor.tracing", redis, 1024)
	schedule.JobHistoryAdaptor = messaging.NewRedisAdaptor[schedule.JobHistory]("monitor.schedule", redis, 1024)
	core.ErrorAdaptor = messaging.NewRedisAdaptor[core.ErrorReport]("monitor.error", redis, 1024)

	logger.Info("monitor adaptors switched to redis streaming")
}

// RunAsAdaptor 从容器解析 messaging 服务并执行 EnableRedisAdaptors。
// 供消费端进程（monitor-adaptor / scm-datapool）在启动服务之前调用。
func RunAsAdaptor() error {
	return core.GetContainer().Invoke(func(logger *zap.Logger, service messaging.MessagingService) {
		EnableRedisAdaptors(service)
	})
}

// EnabledMessagingBridge 以 startup 形式注册 EnableRedisAdaptors，供生产者
// 应用（monitor_messaging 构建标签）在服务启动时把全局 adaptor 切到 redis，
// 业务线程直接 Push 到 redis stream，无需桥接转发。
//
// 注意：若同时开启了 tracing.console，console 订阅发生在容器构建期、可能早于
// 本次切换，会绑定到切换前的 chan adaptor（redis 模式下 console 调试输出会
// 失效）——生产环境不启用 console，影响有限。
func EnabledMessagingBridge() {
	core.ProvideStartup(func(logger *zap.Logger, service messaging.MessagingService) core.Startup {
		EnableRedisAdaptors(service)
		return nil
	})
}
