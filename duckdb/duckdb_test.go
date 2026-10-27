package duckdb

import (
	"errors"
	"regexp"
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

// TestBlobRefLiterals 覆盖外置引用三列的字面量：nil 写 NULL、"key:path" 拼接、size/sha256。
func TestBlobRefLiterals(t *testing.T) {
	if got := blobRefLiteral(nil); got != "NULL" {
		t.Errorf("blobRefLiteral(nil) = %s", got)
	}
	if got := blobRefLiteral(&monitor.BlobRef{Key: "fileroot"}); got != "NULL" {
		t.Errorf("blobRefLiteral(no path) = %s", got)
	}
	ref := &monitor.BlobRef{
		Key:    "fileroot",
		Path:   "magazzin/inventoryExport/202610/inventory_1-20261012153000.xlsx",
		Size:   24000000,
		SHA256: "ab12",
	}
	wantRef := "'fileroot:magazzin/inventoryExport/202610/inventory_1-20261012153000.xlsx'"
	if got := blobRefLiteral(ref); got != wantRef {
		t.Errorf("blobRefLiteral = %s, want %s", got, wantRef)
	}
	if got := blobSizeLiteral(ref); got != "24000000" {
		t.Errorf("blobSizeLiteral = %s", got)
	}
	if got := blobSHA256Literal(ref); got != "'ab12'" {
		t.Errorf("blobSHA256Literal = %s", got)
	}
	if got := blobSizeLiteral(nil); got != "NULL" {
		t.Errorf("blobSizeLiteral(nil) = %s", got)
	}
	if got := blobSHA256Literal(&monitor.BlobRef{Key: "k", Path: "p"}); got != "NULL" {
		t.Errorf("blobSHA256Literal(no sha) = %s", got)
	}
}

// TestBuildTracingInsertIncludesBlobColumns 保证 INSERT 的列与值个数一致、且含外置引用列。
func TestBuildTracingInsertIncludesBlobColumns(t *testing.T) {
	sql := buildTracingInsert("prd_", []monitor.TracingDetails{{
		Optionname: "/scan.InventoryService/Export",
		StartedAt:  time.Date(2026, 10, 12, 15, 30, 0, 0, time.UTC),
		RespBlob: &monitor.BlobRef{
			Key: "fileroot", Path: "owner/cat/202610/x.xlsx", Size: 10, SHA256: "ff",
		},
	}})
	for _, col := range []string{
		"body_blob_ref", "body_blob_size", "body_blob_sha256",
		"resp_blob_ref", "resp_blob_size", "resp_blob_sha256",
	} {
		if !strings.Contains(sql, col) {
			t.Errorf("INSERT 缺少列 %s", col)
		}
	}
	if !strings.Contains(sql, "'fileroot:owner/cat/202610/x.xlsx'") {
		t.Errorf("INSERT 未写入 resp_blob_ref：%s", sql)
	}
	t.Logf("生成的 INSERT = %s", sql)
	// 列数应等于每行值的个数（粗校验：左括号后逗号数一致）。
	cols := strings.Count(tracingColumns, ",") + 1
	values := sql[strings.Index(sql, "VALUES ("):]
	if got := strings.Count(values[:strings.LastIndex(values, ")")], ",") + 1; got != cols {
		t.Errorf("列数 %d != 值个数 %d", cols, got)
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

// TestBuildErrorInsertZeroHappendAT 上报端没填 HappendAT 时不能落 NULL：
// NULL happened_at 既进不了时间范围查询，也不推进「表最后写入时间」（2026-10-27 P1 误报根因）。
func TestBuildErrorInsertZeroHappendAT(t *testing.T) {
	before := time.Now().UTC().Add(-2 * time.Second)
	sql := buildErrorInsert("prd_", []core.ErrorReport{{
		AppName: "scm-saas",
		Uri:     "/decode/ping",
		Error:   errors.New("context deadline exceeded"),
		// HappendAT 零值
	}})

	if strings.Contains(sql, "(NULL, ") {
		t.Fatalf("零值 HappendAT 不应落 NULL:\n%s", sql)
	}
	m := regexp.MustCompile(`'([^']*)'::TIMESTAMPTZ`).FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("零值 HappendAT 应退化为入库时刻字面量:\n%s", sql)
	}
	got, err := time.Parse("2006-01-02 15:04:05.999999Z07:00", m[1])
	if err != nil {
		t.Fatalf("时间字面量解析失败 %q: %v", m[1], err)
	}
	// 退化值必须落在「刚刚」附近，而不是某个历史常量（如 1754-08-30 那类坏值）。
	if got.Before(before) || got.After(time.Now().UTC().Add(2*time.Second)) {
		t.Errorf("退化时间 %s 不在当前时刻附近", got)
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
