package monitor

import (
	"context"
	"testing"
)

// TestBlobRefsRoundTrip 覆盖「中间件挂载收集器 → 业务回填 → 中间件读取」的完整链路。
func TestBlobRefsRoundTrip(t *testing.T) {
	ctx := context.Background()

	// 未挂载时应返回 nil，且便捷函数静默忽略（不能 panic）。
	if refs := BlobRefsFrom(ctx); refs != nil {
		t.Fatalf("未挂载时 BlobRefsFrom 应返回 nil，实际 %v", refs)
	}
	AddRespBlob(ctx, BlobRef{Key: "fileroot", Path: "x"})

	ctx, refs := WithBlobRefs(ctx)
	if got := BlobRefsFrom(ctx); got != refs {
		t.Fatalf("BlobRefsFrom 未返回挂载的收集器")
	}
	if refs.FirstBody() != nil || refs.FirstResp() != nil {
		t.Fatalf("空收集器应返回 nil")
	}

	refs.AddResp(BlobRef{Key: "fileroot", Path: "owner/cat/202610/a.xlsx", Size: 11, SHA256: "aa"})
	AddRespBlob(ctx, BlobRef{Key: "fileroot", Path: "owner/cat/202610/b.xlsx", Size: 22})
	refs.AddBody(BlobRef{Key: "fileroot", Path: "owner/cat/202610/in.xlsx"})
	// 空 Path 必须被忽略（避免写入无意义的引用）。
	refs.AddResp(BlobRef{Key: "fileroot"})

	if b, r := refs.Size(); b != 1 || r != 2 {
		t.Fatalf("Size() = (%d,%d)，期望 (1,2)", b, r)
	}
	first := refs.FirstResp()
	if first == nil || first.Path != "owner/cat/202610/a.xlsx" || first.Size != 11 {
		t.Fatalf("FirstResp 应返回首个引用，实际 %+v", first)
	}
	if firstBody := refs.FirstBody(); firstBody == nil || firstBody.Path != "owner/cat/202610/in.xlsx" {
		t.Fatalf("FirstBody 异常：%+v", firstBody)
	}
	// 返回的是副本：改动不应影响收集器内部状态。
	first.Size = 999
	if again := refs.FirstResp(); again.Size != 11 {
		t.Fatalf("FirstResp 应返回副本，实际 Size=%d", again.Size)
	}
}
