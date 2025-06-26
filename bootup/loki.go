//go:build monitor_default || monitor_loki

package bootup

import "github.com/techquest-tech/monitor/loki"

func init() {
	loki.EnableLokiMonitor()
}
