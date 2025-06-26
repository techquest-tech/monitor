//go:build monitor_default || monitor_insights

package bootup

import "github.com/techquest-tech/monitor/insights"

func init() {
	insights.EnabledMonitor()
}
