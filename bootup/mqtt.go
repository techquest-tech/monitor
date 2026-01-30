//go:build monitor_default || monitor_mqtt

package bootup

import "github.com/techquest-tech/monitor/mqtt"

func init() {
	mqtt.EnableMqttSource()
}
