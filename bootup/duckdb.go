//go:build monitor_duckdb

package bootup

import "github.com/techquest-tech/monitor/duckdb"

// init 注册 duckdb 消费者。与 monitor_db 等 receiver 相同的 opt-in 约定：
// 仅当构建带 monitor_duckdb tag 时才进入 binary；即使进入，tracing.duckdb
// 配置缺失 / enabled=false / token 为空时 EnableDuckDBMonitor 内也不会真正
// 订阅（InitDuckDBMonitor 返回 nil，receiver 不启动）。
func init() {
	duckdb.EnableDuckDBMonitor()
}
