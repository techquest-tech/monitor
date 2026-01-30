package mqtt

import (
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/mqttclient"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

type MqttConfig struct {
	Topic []string
}

type MqttSource struct {
	Config *MqttConfig
	Client *mqttclient.MqttService
	Logger *zap.Logger
}

func NewMqttSource(logger *zap.Logger, client *mqttclient.MqttService) *MqttSource {
	settings := viper.Sub("tracing.mqtt")
	if settings == nil {
		return nil
	}
	conf := &MqttConfig{}
	err := settings.Unmarshal(conf)
	if err != nil {
		logger.Error("failed to unmarshal mqtt config", zap.Error(err))
		return nil
	}

	return &MqttSource{
		Config: conf,
		Logger: logger,
		Client: client,
	}
}

func (ms *MqttSource) Start() {
	if len(ms.Config.Topic) == 0 {
		ms.Logger.Warn("mqtt tracing source enabled, but no topic configured")
		return
	}

	for _, topic := range ms.Config.Topic {
		err := ms.Client.Sub("$share/monitor/"+topic, ms.onMessage)
		if err != nil {
			ms.Logger.Error("failed to subscribe topic", zap.String("topic", topic), zap.Error(err))
		} else {
			ms.Logger.Info("subscribed to topic", zap.String("topic", topic))
		}
	}
}

func (ms *MqttSource) onMessage(client mqtt.Client, msg mqtt.Message) {
	details := monitor.TracingDetails{
		Optionname: msg.Topic(),
		Uri:        "mqtt://" + msg.Topic(),
		Method:     "MQTT",
		Body:       string(msg.Payload()),
		Durtion:    0, // Message receipt is instantaneous in this context
		Status:     200,
		StartedAt:  time.Now(),
		// Fill other fields if necessary
	}
	monitor.TracingAdaptor.Push(details)
}

func EnableMqttSource() {
	core.Provide(NewMqttSource)
	core.ProvideStartup(func(ms *MqttSource) core.Startup {
		if ms != nil {
			ms.Start()
		}
		return nil
	})
}
