package duckdb

import (
	"strings"
	"testing"
	"time"

	"github.com/techquest-tech/monitor"
)

func TestBuildConnectionInsert(t *testing.T) {
	at := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	sql := buildConnectionInsert("prd_", []monitor.ConnectionDetails{{
		Action:     monitor.ConnectionClose,
		HappenedAt: at,
		ConnID:     "c9",
		StartedAt:  at.Add(-90 * time.Minute),
		Duration:   90 * time.Minute,
		RPCs:       3,
		Peer:       "10.0.0.1:41234",
		LocalAddr:  "10.0.0.2:3001",
		ClientIP:   "1.2.3.4",
		Client:     "android_handheld_scm_v1.2.3",
		Device:     "DT50-RoyalRay-Common/abc/SN-1",
		SN:         "SN-1",
		AppName:    "scm",
		AppVersion: "v20261017.1",
		Tenant:     "esquel",
		Reason:     "too_many_pings",
	}})

	if !strings.HasPrefix(sql, "INSERT INTO prd_monitor_connection ") {
		t.Fatalf("bad prefix: %s", sql)
	}
	for _, want := range []string{
		"'close'", "'c9'", "5400000", "'too_many_pings'", "'1.2.3.4'", "'SN-1'", "'android_handheld_scm_v1.2.3'",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in:\n%s", want, sql)
		}
	}
	// 16 列 → VALUES 里单行 15 个分隔符（fixture 里没有带逗号的字符串）。
	_, values, ok := strings.Cut(sql, " VALUES ")
	if !ok {
		t.Fatalf("no VALUES clause: %s", sql)
	}
	if got := strings.Count(values, ", "); got != 15 {
		t.Errorf("separator count = %d, want 15 (16 columns); values=%s", got, values)
	}
}

// TestConnectionTableDDLRegistered 守住建表语句：表名、关键列必须与
// connectionColumns / buildConnectionInsert 一致，否则写入会整天失败。
func TestConnectionTableDDLRegistered(t *testing.T) {
	s := &DuckDBBatchService{config: &Config{TablePrefix: "prd_"}}
	var ddl string
	for _, d := range s.ddlStatements() {
		if strings.Contains(d, "monitor_connection") {
			ddl = d
		}
	}
	if ddl == "" {
		t.Fatal("monitor_connection DDL is not registered")
	}
	for _, col := range []string{
		"happened_at", "action", "conn_id", "started_at", "duration_ms", "rpcs",
		"peer", "local_addr", "client_ip", "client", "device", "sn",
		"app_name", "app_version", "tenant", "reason",
	} {
		if !strings.Contains(connectionColumns, col) {
			t.Errorf("connectionColumns missing %q", col)
		}
		if !strings.Contains(ddl, col+" ") {
			t.Errorf("DDL missing column %q: %s", col, ddl)
		}
	}
}
