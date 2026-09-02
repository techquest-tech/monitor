package duckdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

// TestLiveQuack 端到端验证（可选）：对真实 quack 服务器执行批量写入并回读。
// 需要环境变量 QUACK_LIVE_ENDPOINT / QUACK_LIVE_TOKEN（本地测试服务器即可，
// 勿指向生产实例——本测试会建 e2e_ 前缀的表并写入数据）。
func TestLiveQuack(t *testing.T) {
	endpoint := os.Getenv("QUACK_LIVE_ENDPOINT")
	token := os.Getenv("QUACK_LIVE_TOKEN")
	if endpoint == "" || token == "" {
		t.Skip("set QUACK_LIVE_ENDPOINT and QUACK_LIVE_TOKEN to run live test")
	}

	// 每次运行使用唯一表前缀，避免复用本地库历史数据
	prefix := fmt.Sprintf("e2e_%d_", time.Now().UnixNano()%100000000)
	cfg := &Config{
		Enabled:        true,
		Endpoint:       endpoint,
		Token:          token,
		TablePrefix:    prefix,
		WriteTimeoutMS: 15000,
	}
	s := NewDuckDBBatchService(cfg, zap.NewNop())
	defer s.Close(context.Background())

	// 批量写 tracing（含单引号/二进制/零时间边界）
	err := s.ReportTracingBatch([]monitor.TracingDetails{
		{
			Optionname:     "/api/order",
			Uri:            "/api/order/1",
			Method:         "GET",
			AppName:        "e2e-app",
			AppVersion:     "v1",
			VerbosityLevel: monitor.TracingVerbosityLevelRead,
			Body:           []byte{0xDE, 0xAD, 0xBE, 0xEF},
			BodyEnc:        "json",
			Durtion:        321 * time.Millisecond,
			Status:         200,
			TargetID:       7,
			Resp:           nil,
			RespEnc:        "",
			ClientIP:       "10.1.2.3",
			UserAgent:      "it's-e2e",
			Device:         "d",
			Tenant:         "esquel",
			Operator:       "panarm",
			StartedAt:      time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		},
		{
			Optionname: "/api/second",
			Uri:        "/api/second",
			Method:     "POST",
			AppName:    "e2e-app",
			StartedAt:  time.Time{},
		},
	})
	if err != nil {
		t.Fatalf("tracing batch: %v", err)
	}

	// 批量写 error（nil error + 堆栈）
	if err := s.ReportErrorBatch([]core.ErrorReport{
		{
			AppName:    "e2e-app",
			AppVersion: "v1",
			Uri:        "/api/boom",
			FullStack:  []byte("goroutine stack\nline2"),
			Error:      errors.New("boom it's broken"),
			HappendAT:  time.Date(2026, 9, 1, 10, 1, 0, 0, time.UTC),
		},
		{AppName: "e2e-app", Uri: "/api/ok"},
	}); err != nil {
		t.Fatalf("error batch: %v", err)
	}

	// 批量写 job history
	if err := s.ReportScheduleJobBatch([]schedule.JobHistory{
		{
			App:        "e2e-app",
			AppVersion: "v1",
			Job:        "cleanup",
			Cron:       "11 2 * * 0",
			Start:      time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC),
			Finished:   time.Date(2026, 9, 1, 2, 0, 5, 0, time.UTC),
			Next:       time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC),
			Duration:   5 * time.Second,
			Succeed:    true,
			Message:    "done",
			Disabled:   false,
		},
	}); err != nil {
		t.Fatalf("job batch: %v", err)
	}

	// 回读核对
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checks := []struct {
		query string
		want  int64
	}{
		{"SELECT count(*) FROM " + prefix + "monitor_tracing", 2},
		{"SELECT count(*) FROM " + prefix + "monitor_error", 2},
		{"SELECT count(*) FROM " + prefix + "monitor_job_history", 1},
	}
	for _, c := range checks {
		v, err := s.queryValue(ctx, c.query)
		if err != nil {
			t.Fatalf("query %s: %v", c.query, err)
		}
		n, ok := v.(int64)
		if !ok {
			t.Fatalf("query %s returned non-int64: %v (%T)", c.query, v, v)
		}
		if n != c.want {
			t.Errorf("query %s = %d, want %d", c.query, n, c.want)
		}
	}

	// 转义与二进制往返：读回 optionname 含单引号的行与 body hex
	opt, err := s.queryValue(ctx,
		"SELECT optionname FROM "+prefix+"monitor_tracing WHERE uri = '/api/order/1'")
	if err != nil {
		t.Fatalf("readback optionname: %v", err)
	}
	if opt != "/api/order" {
		t.Errorf("readback optionname = %v", opt)
	}
	enc, err := s.queryValue(ctx,
		"SELECT body_enc FROM "+prefix+"monitor_tracing WHERE uri = '/api/order/1'")
	if err != nil {
		t.Fatalf("readback body_enc: %v", err)
	}
	if enc != "json" {
		t.Errorf("readback body_enc = %v", enc)
	}

	// 零值时间 → NULL 行也应存在
	n2, err := s.queryValue(ctx,
		"SELECT count(*) FROM "+prefix+"monitor_tracing WHERE started_at IS NULL")
	if err != nil {
		t.Fatalf("null-time query: %v", err)
	}
	if n2 != int64(1) {
		t.Errorf("rows with NULL started_at = %v, want 1", n2)
	}
}

// TestLiveCleanup 验证分级清理 DELETE：插入一条旧记录（started_at 早于 cutoff）
// 与一条新记录，执行 CleanTier 后旧记录被删、新记录保留。
func TestLiveCleanup(t *testing.T) {
	endpoint := os.Getenv("QUACK_LIVE_ENDPOINT")
	token := os.Getenv("QUACK_LIVE_TOKEN")
	if endpoint == "" || token == "" {
		t.Skip("set QUACK_LIVE_ENDPOINT and QUACK_LIVE_TOKEN to run live test")
	}
	prefix := fmt.Sprintf("e2e_cl_%d_", time.Now().UnixNano()%100000000)
	s := NewDuckDBBatchService(&Config{
		Enabled: true, Endpoint: endpoint, Token: token,
		TablePrefix: prefix, WriteTimeoutMS: 15000,
	}, zap.NewNop())
	defer s.Close(context.Background())

	old := time.Now().AddDate(0, -8, 0)  // 8 个月前（早于 6 个月 cutoff）
	recent := time.Now().Add(-time.Hour) // 1 小时前（保留）
	batch := []monitor.TracingDetails{
		{Optionname: "/old", Uri: "/old", Method: "GET", VerbosityLevel: monitor.TracingVerbosityLevelMostImportant, StartedAt: old},
		{Optionname: "/recent", Uri: "/recent", Method: "GET", VerbosityLevel: monitor.TracingVerbosityLevelMostImportant, StartedAt: recent},
	}
	if err := s.ReportTracingBatch(batch); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cl := &tracingCleaner{svc: s}
	// 档1：verbosity ∈ (0,10]，cutoff = now-6 个月 → 应删除 old，保留 recent
	n, err := cl.CleanTier(context.Background(), -1, 10, time.Now().AddDate(0, -6, 0))
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 deleted, got %d", n)
	}
	remain, err := s.queryValue(context.Background(),
		"SELECT count(*) FROM "+prefix+"monitor_tracing")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if remain != int64(1) {
		t.Errorf("expected 1 remaining row, got %v", remain)
	}
	// 二次清理应删除 0 行
	n2, err := cl.CleanTier(context.Background(), -1, 10, time.Now().AddDate(0, -6, 0))
	if err != nil {
		t.Fatalf("clean2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("expected 0 deleted on second run, got %d", n2)
	}
}
