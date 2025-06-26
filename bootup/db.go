//go:build monitor_db

package bootup

import "github.com/techquest-tech/monitor/db"

func init() {
	db.EnableDBMonitor()
}
