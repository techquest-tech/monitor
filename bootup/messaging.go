//go:build !monitor_default && monitor_messaging

package bootup

import "github.com/techquest-tech/monitor/messaging"

func init() {
	messaging.EnabledMessagingBridge()
}
