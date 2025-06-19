//go:build monitor_default || monitor_datapool

package bootup

import "github.com/techquest-tech/monitor/datapool"

func init() {
	datapool.Enabled2Datapool()
}
