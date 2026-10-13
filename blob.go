package monitor

import (
	"context"
	"sync"
)

// blobRefsKey 是 context 中承载 *BlobRefs 的私有键类型（避免与其他包的键冲突）。
type blobRefsKey struct{}

// BlobRefs 是「请求范围内可写」的引用收集器，用于把「内容已落存储」的事实
// 从业务处理传递回埋点中间件。
//
// 用法（生产端）：
//  1. 埋点中间件在调用业务处理之前：ctx, refs := monitor.WithBlobRefs(ctx)，
//     并把该 ctx 交给业务处理（gRPC 拦截器包装 ServerStream；gin 侧可 c.Set）；
//  2. 业务或工具在同一个 ctx 上把内容落盘后：monitor.AddBodyBlob(ctx, ref) /
//     monitor.AddRespBlob(ctx, ref)（持有收集器时也可 refs.AddBody(ref)）；
//  3. 中间件在处理返回后取 refs.FirstResp()/FirstBody()，据此决定该条 trace
//     记录「内容」还是「引用」——记录引用时不要再去 marshal 大对象。
//
// 并发安全：落盘通常在独立 goroutine 中完成，读写均加锁。
type BlobRefs struct {
	mu   sync.Mutex
	body []BlobRef
	resp []BlobRef
}

// AddBody 记录一个请求体引用（Path 为空时忽略）。
func (b *BlobRefs) AddBody(ref BlobRef) { b.add(&b.body, ref) }

// AddResp 记录一个响应体引用（Path 为空时忽略）。
func (b *BlobRefs) AddResp(ref BlobRef) { b.add(&b.resp, ref) }

func (b *BlobRefs) add(dst *[]BlobRef, ref BlobRef) {
	if b == nil || ref.Path == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	*dst = append(*dst, ref)
}

// FirstBody 返回首个请求体引用（无则 nil）。
func (b *BlobRefs) FirstBody() *BlobRef { return b.first(&b.body) }

// FirstResp 返回首个响应体引用（无则 nil）。
func (b *BlobRefs) FirstResp() *BlobRef { return b.first(&b.resp) }

// first 返回切片首元素的副本；调用方无需担心后续追加导致指针失效。
func (b *BlobRefs) first(src *[]BlobRef) *BlobRef {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(*src) == 0 {
		return nil
	}
	ref := (*src)[0]
	return &ref
}

// Size 返回已收集的体/响应引用数量（body, resp），便于中间件判断是否为批量落盘。
func (b *BlobRefs) Size() (int, int) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.body), len(b.resp)
}

// WithBlobRefs 在 ctx 上挂一个空的收集器，返回派生 ctx 与收集器。
func WithBlobRefs(ctx context.Context) (context.Context, *BlobRefs) {
	refs := &BlobRefs{}
	return context.WithValue(ctx, blobRefsKey{}, refs), refs
}

// BlobRefsFrom 取出 ctx 上的收集器；未挂载时返回 nil（调用方需判空）。
func BlobRefsFrom(ctx context.Context) *BlobRefs {
	if ctx == nil {
		return nil
	}
	refs, _ := ctx.Value(blobRefsKey{}).(*BlobRefs)
	return refs
}

// AddBodyBlob 是给「只拿到 ctx」的调用方（如文件落盘工具）使用的便捷函数；
// 未挂载收集器时静默忽略。
func AddBodyBlob(ctx context.Context, ref BlobRef) {
	if refs := BlobRefsFrom(ctx); refs != nil {
		refs.AddBody(ref)
	}
}

// AddRespBlob 同 AddBodyBlob，用于响应体。
func AddRespBlob(ctx context.Context, ref BlobRef) {
	if refs := BlobRefsFrom(ctx); refs != nil {
		refs.AddResp(ref)
	}
}
