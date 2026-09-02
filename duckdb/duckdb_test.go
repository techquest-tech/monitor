package duckdb

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

func TestSQLString(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc", "'abc'"},
		{"it's", "'it''s'"},
		{`a\b`, `'a\b'`}, // 标准字符串反斜杠原样
		{"", "''"},
		{"a'b'c", "'a''b''c'"},
	}
	for _, c := range cases {
		if got := sqlString(c.in); got != c.want {
			t.Errorf("sqlString(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestBlobLiteral(t *testing.T) {
	if got := blobLiteral(nil); got != "NULL" {
		t.Errorf("blobLiteral(nil) = %s", got)
	}
	if got := blobLiteral([]byte{}); got != "NULL" {
		t.Errorf("blobLiteral(empty) = %s", got)
	}
	if got := blobLiteral([]byte{0xAB, 0x01}); got != "from_hex('ab01')" {
		t.Errorf("blobLiteral = %s", got)
	}
}

func TestTsLiteral(t *testing.T) {
	if got := tsLiteral(time.Time{}); got != "NULL" {
		t.Errorf("tsLiteral(zero) = %s", got)
	}
	// UTC 时刻应输出 Z 结尾的 TIMESTAMPTZ 字面量
	ts := time.Date(2026, 9, 1, 12, 30, 0, 123456000, time.UTC)
	if got := tsLiteral(ts); got != "'2026-09-01 12:30:00.123456Z'::TIMESTAMPTZ" {
		t.Errorf("tsLiteral = %s", got)
	}
	// 非 UTC 输入统一转 UTC
	sh := time.FixedZone("+08", 8*3600)
	ts2 := time.Date(2026, 9, 1, 20, 30, 0, 0, sh)
	if got := tsLiteral(ts2); got != "'2026-09-01 12:30:00Z'::TIMESTAMPTZ" {
		t.Errorf("tsLiteral(+08:00) = %s", got)
	}
}

func TestDurationMs(t *testing.T) {
	if got := durationMs(1500 * time.Millisecond); got != 1500 {
		t.Errorf("durationMs = %d", got)
	}
	if got := durationMs(2 * time.Second); got != 2000 {
		t.Errorf("durationMs = %d", got)
	}
}

func TestBoolLiteral(t *testing.T) {
	if got := boolLiteral(true); got != "TRUE" {
		t.Errorf("boolLiteral(true) = %s", got)
	}
	if got := boolLiteral(false); got != "FALSE" {
		t.Errorf("boolLiteral(false) = %s", got)
	}
}

func TestBuildTracingInsert(t *testing.T) {
	trs := []monitor.TracingDetails{
		{
			Optionname:     "/api/order",
			Uri:            "/api/order/1",
			Method:         "GET",
			AppName:        "scm-order",
			AppVersion:     "v1",
			VerbosityLevel: monitor.TracingVerbosityLevelRead,
			Body:           []byte{0x01, 0x02},
			BodyEnc:        "json",
			Durtion:        123 * time.Millisecond,
			Status:         200,
			TargetID:       42,
			Resp:           nil,
			RespEnc:        "",
			ClientIP:       "10.0.0.1",
			UserAgent:      "it's-a-test",
			Device:         "d1",
			Tenant:         "esquel",
			Operator:       "panarm",
			StartedAt:      time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC),
		},
		{
			Optionname: "/api/x",
			Uri:        "/api/x",
			Method:     "POST",
			AppName:    "",
			StartedAt:  time.Time{},
		},
	}
	sql := buildTracingInsert("prd_", trs)
	for _, want := range []string{
		"INSERT INTO prd_monitor_tracing",
		"(started_at, optionname, uri, method",
		"'/api/order/1'",
		"'it''s-a-test'",
		"from_hex('0102')",
		"123",
		"99", // verbosity read
		"200",
		"42",
		"NULL", // resp 空 → NULL
		"'2026-09-01 08:00:00Z'::TIMESTAMPTZ",
		", (NULL, '/api/x'",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("buildTracingInsert missing %q in:\n%s", want, sql)
		}
	}
	// 两条记录 → 两行 VALUES
	if got := strings.Count(sql, "), (") + strings.Count(sql, "),("); got != 1 {
		t.Errorf("expected 2 value rows, separators found = %d\n%s", got, sql)
	}
}

func TestBuildErrorInsert(t *testing.T) {
	rrs := []core.ErrorReport{
		{
			AppName:    "monitor-adaptor",
			AppVersion: "v0.0.1",
			Uri:        "/api/boom",
			FullStack:  []byte("stack\nline2"),
			Error:      errors.New("boom: it's broken"),
			HappendAT:  time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
		},
		{
			AppName: "x",
			Uri:     "/api/ok",
			Error:   nil,
		},
	}
	sql := buildErrorInsert("prd_", rrs)
	for _, want := range []string{
		"INSERT INTO prd_monitor_error",
		"boom: it''s broken",
		"from_hex('737461636b0a6c696e6532')",
		"'2026-09-01 09:00:00Z'::TIMESTAMPTZ",
		"''", // nil error → 空串
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("buildErrorInsert missing %q in:\n%s", want, sql)
		}
	}
}

func TestBuildJobHistoryInsert(t *testing.T) {
	reqs := []schedule.JobHistory{
		{
			App:        "monitor-adaptor",
			AppVersion: "v1",
			Job:        "monitor_db_cleanup",
			Cron:       "11 2 * * 0",
			Start:      time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC),
			Finished:   time.Date(2026, 9, 1, 2, 0, 5, 0, time.UTC),
			Next:       time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC),
			Duration:   5 * time.Second,
			Succeed:    true,
			Message:    "done",
			Disabled:   false,
		},
	}
	sql := buildJobHistoryInsert("uat_", reqs)
	for _, want := range []string{
		"INSERT INTO uat_monitor_job_history",
		"'monitor_db_cleanup'",
		"5000",
		"TRUE",
		"FALSE",
		"'2026-09-01 02:00:00Z'::TIMESTAMPTZ",
		"'2026-09-08 02:00:00Z'::TIMESTAMPTZ",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("buildJobHistoryInsert missing %q in:\n%s", want, sql)
		}
	}
}

// TestInsertIsPlainSQL 验证批量 INSERT 是不含 ATTACH/r.query 包装的裸 SQL
// （duckcall native API 直接提交给服务器主库执行）。
func TestInsertIsPlainSQL(t *testing.T) {
	trs := []monitor.TracingDetails{{
		Optionname: "/api/order", Uri: "/api/order/1", Method: "GET",
		StartedAt: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC),
	}}
	sql := buildTracingInsert("", trs)
	if strings.Contains(sql, "r.query") || strings.Contains(sql, "ATTACH") {
		t.Errorf("expected plain SQL without remote wrapper: %s", sql)
	}
	if !strings.HasPrefix(sql, "INSERT INTO monitor_tracing") {
		t.Errorf("bad prefix: %s", sql)
	}
}

// TestInitDuckDBMonitorGating 验证 receiver 启动门控（用户要求：配置缺失/未
// enabled/token 为空时均不启动 receiver）。
func TestInitDuckDBMonitorGating(t *testing.T) {
	logger := zap.NewNop()
	viper.Reset()

	// 1) 配置完全缺失 → 不启动
	s, err := InitDuckDBMonitor(logger)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if s != nil {
		t.Error("config missing: expected nil service")
	}

	// 2) enabled 但 token 缺失 → 不启动
	viper.Set("tracing.duckdb.enabled", true)
	s, err = InitDuckDBMonitor(logger)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if s != nil {
		t.Error("token missing: expected nil service")
	}

	// 3) enabled + token → 启动
	viper.Set("tracing.duckdb.token", "testtoken")
	s, err = InitDuckDBMonitor(logger)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if s == nil {
		t.Error("enabled+token: expected service")
	}
	if s.config.Endpoint != "localhost:9494" {
		t.Errorf("default endpoint = %q", s.config.Endpoint)
	}

	viper.Reset()
}

// TestBuildCleanTierDeleteSQL 验证分级删除 SQL（前缀/区间/时间字面量）。
func TestBuildCleanTierDeleteSQL(t *testing.T) {
	cutoff := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	sql := buildCleanTierDeleteSQL("uat_", 10, 50, cutoff)
	want := "DELETE FROM uat_monitor_tracing WHERE started_at <= '2026-03-01 02:00:00Z'::TIMESTAMPTZ AND verbosity_level > 10 AND verbosity_level <= 50"
	if sql != want {
		t.Errorf("buildCleanTierDeleteSQL =\n  %s\nwant\n  %s", sql, want)
	}
	// 默认前缀
	sql = buildCleanTierDeleteSQL("", 50, 2147483647, cutoff)
	if !strings.HasPrefix(sql, "DELETE FROM monitor_tracing WHERE") {
		t.Errorf("default prefix SQL = %s", sql)
	}
	if !strings.Contains(sql, "verbosity_level > 50") {
		t.Errorf("missing >50 clause: %s", sql)
	}
}
