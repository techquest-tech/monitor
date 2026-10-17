package mqtt

import (
	"testing"

	mqttlib "github.com/eclipse/paho.mqtt.golang"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

type fakeMessage struct {
	topic   string
	payload []byte
}

func (m fakeMessage) Duplicate() bool { return false }
func (m fakeMessage) Qos() byte       { return 0 }
func (m fakeMessage) Retained() bool  { return false }
func (m fakeMessage) Topic() string   { return m.topic }
func (m fakeMessage) MessageID() uint16 {
	return 0
}
func (m fakeMessage) Payload() []byte { return m.payload }
func (m fakeMessage) Ack()            {}

var _ mqttlib.Message = fakeMessage{}

// recorderAdaptor 记录 Push 的 tracing，便于断言。
type recorderAdaptor struct {
	pushes []monitor.TracingDetails
}

func (r *recorderAdaptor) Push(data monitor.TracingDetails) { r.pushes = append(r.pushes, data) }
func (r *recorderAdaptor) Sub(receiver string) <-chan monitor.TracingDetails {
	return nil
}
func (r *recorderAdaptor) Subscripter(receiver string, fn core.Handler[monitor.TracingDetails]) {
}
func (r *recorderAdaptor) Start()              {}
func (r *recorderAdaptor) Stop()               {}
func (r *recorderAdaptor) Receivers() []string { return nil }

var _ core.Adaptor[monitor.TracingDetails] = (*recorderAdaptor)(nil)

func TestOnMessageSkippedWhenTracingDisabled(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	rec := &recorderAdaptor{}
	old := monitor.TracingAdaptor
	monitor.TracingAdaptor = rec
	t.Cleanup(func() { monitor.TracingAdaptor = old })

	ms := &MqttSource{Logger: zap.NewNop()}
	msg := fakeMessage{topic: "demo/topic", payload: []byte("payload")}

	viper.Set("tracing.enabled", false)
	ms.onMessage(nil, msg)
	assert.Empty(t, rec.pushes, "关闭 tracing 时不应推送")

	viper.Set("tracing.enabled", true)
	ms.onMessage(nil, msg)
	require.Len(t, rec.pushes, 1)
	assert.Equal(t, "demo/topic", rec.pushes[0].Optionname)
}

func TestStartSkippedWhenTracingDisabled(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("tracing.enabled", false)

	// Config / Client 都为 nil：若没有在访问它们之前返回，这里会 panic。
	ms := &MqttSource{Logger: zap.NewNop()}
	assert.NotPanics(t, ms.Start)
}
