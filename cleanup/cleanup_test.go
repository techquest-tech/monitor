package cleanup

import (
	"context"
	"math"
	"testing"
	"time"
)

// fakeCleaner 记录每次 CleanTier 收到的参数，可按需返回删除数/错误。
type fakeCleaner struct {
	calls   []tierCall
	deleted []int64
	errs    []error
}

type tierCall struct {
	min, max int
	cutoff   time.Time
}

func (f *fakeCleaner) CleanTier(_ context.Context, minExclusive, max int, cutoff time.Time) (int64, error) {
	idx := len(f.calls)
	f.calls = append(f.calls, tierCall{minExclusive, max, cutoff})
	if idx < len(f.deleted) {
		return f.deleted[idx], nil
	}
	if idx < len(f.errs) {
		return 0, f.errs[idx]
	}
	return 0, nil
}

func TestDefaultTracingPolicy(t *testing.T) {
	p := DefaultTracingPolicy()
	if len(p.Tiers) != 3 {
		t.Fatalf("expected 3 tiers, got %d", len(p.Tiers))
	}
	want := []Tier{{10, 6, 0}, {50, 0, 14}, {math.MaxInt, 0, 3}}
	for i, w := range want {
		if p.Tiers[i] != w {
			t.Errorf("tier[%d] = %+v, want %+v", i, p.Tiers[i], w)
		}
	}
}

func TestRunTiersAndCutoffs(t *testing.T) {
	f := &fakeCleaner{deleted: []int64{100, 200, 300}}
	// 记录 Run 执行前的 now，用于按日历语义精确校验 cutoff
	before := time.Now()
	results := Run(context.Background(), f, DefaultTracingPolicy())

	if len(f.calls) != 3 {
		t.Fatalf("expected 3 CleanTier calls, got %d", len(f.calls))
	}
	// 区间：verbosity ∈ (prev, max]
	wantRanges := [][2]int{{-1, 10}, {10, 50}, {50, math.MaxInt}}
	for i, r := range wantRanges {
		if f.calls[i].min != r[0] || f.calls[i].max != r[1] {
			t.Errorf("call[%d] range = (%d,%d], want (%d,%d]", i, f.calls[i].min, f.calls[i].max, r[0], r[1])
		}
	}
	// cutoff：6 个自然月 / 14 天 / 3 天（与 todb 原 AddDate 日历语义一致）
	wantCutoffs := []time.Time{
		before.AddDate(0, -6, 0),
		before.AddDate(0, 0, -14),
		before.AddDate(0, 0, -3),
	}
	for i, w := range wantCutoffs {
		c := f.calls[i].cutoff
		// 允许 Run 内部取 now 与 before 之间的时钟偏差（秒级）
		if c.Before(w.Add(-2*time.Second)) || c.After(w.Add(2*time.Second)) {
			t.Errorf("call[%d] cutoff = %v, want ≈ %v (within ±2s)", i, c, w)
		}
	}
	// 结果汇总
	if results[0].Deleted != 100 || results[1].Deleted != 200 || results[2].Deleted != 300 {
		t.Errorf("results deleted mismatch: %+v", results)
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v", i, r.Err)
		}
	}
}

func TestRunErrorPropagation(t *testing.T) {
	f := &fakeCleaner{errs: []error{errBoom{}}}
	results := Run(context.Background(), f, DefaultTracingPolicy())
	if results[0].Err == nil {
		t.Error("expected first tier error to propagate")
	}
	if results[0].Err.Error() != "boom" {
		t.Errorf("unexpected error: %v", results[0].Err)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
