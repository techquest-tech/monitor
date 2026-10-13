package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/afero"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/storage"
	"go.uber.org/zap"
)

// 大字段按大小外溢到 storage（2026-10-12）。
//
// 背景：单条 trace 的 body/resp 实测可达 24 MB（RFID-SCM `/scan.InventoryService/Export`
// 导出的 xlsx 整份内联回传），全量经 Redis Stream → DuckDB 会把内存有限的实例打满
// （2026-10-12 monitor.tracing 涨到 745 MiB 打满 1 GiB 实例，上游静默 111 分钟）。
//
// 与「业务自己落盘」的引用机制（BlobRefs / WithBlobRefs）互补：业务落盘时走那条路
// （零额外上传）；业务没落盘的大字段——如盘点/质检这类查询响应——由本文件按大小兜底，
// 内容写到 storage 留档待查，trace 记录只保留引用。
//
// 配置（viper，均在 tracing.blob 下）：
//
//	tracing.blob.enabled    bool  是否启用（缺省 true）
//	tracing.blob.threshold  int   触发字节数（缺省 131072 = 128 KiB）
//	tracing.blob.key        string storage 配置键（缺省 "fileroot"）
//	tracing.blob.prefix     string 该键根目录下的前缀（缺省 "traceblob"）
//
// 失败策略：写存储失败时**保留内容本身**（不截断、不丢数据）并限频记 error 日志 ——
// 宁可让 streamgc 兜内存，也不静默丢待查记录。
const (
	defaultSpillThreshold = 128 * 1024
	defaultSpillKey       = "fileroot"
	defaultSpillPrefix    = "traceblob"
	spillErrorInterval    = time.Minute
)

type blobSpillConfig struct {
	Enabled   bool
	Threshold int
	Key       string
	Prefix    string
}

// loadBlobSpillConfig 读取 tracing.blob 配置并套用缺省值（每次调用都读，便于改配置后重启生效；
// 用 viper.Get* 而非 viper.Sub，这样测试与运行期 viper.Set 覆盖同样生效）。
func loadBlobSpillConfig() blobSpillConfig {
	cfg := blobSpillConfig{
		Enabled:   true,
		Threshold: defaultSpillThreshold,
		Key:       defaultSpillKey,
		Prefix:    defaultSpillPrefix,
	}
	if viper.IsSet("tracing.blob.enabled") {
		cfg.Enabled = viper.GetBool("tracing.blob.enabled")
	}
	if v := viper.GetInt("tracing.blob.threshold"); v > 0 {
		cfg.Threshold = v
	}
	if v := strings.TrimSpace(viper.GetString("tracing.blob.key")); v != "" {
		cfg.Key = v
	}
	if v := strings.TrimSpace(viper.GetString("tracing.blob.prefix")); v != "" {
		cfg.Prefix = strings.TrimPrefix(v, "/")
	}
	return cfg
}

// SpillIfLarge 在 data 超过阈值时写入 storage 并返回引用；未启用、未超阈值或写失败返回 nil。
//
// kind 取 "body" / "resp"（仅用于文件命名与日志）；nameHint 建议传操作名或 trace ID，
// 仅影响可读性（文件名末尾另有内容 sha 前 8 位，保证不覆盖）。
// 成功时同时把引用登记到 ctx 上的收集器（若已挂载），便于同请求内其它分量复用。
func SpillIfLarge(ctx context.Context, kind string, data []byte, nameHint string) *BlobRef {
	if len(data) == 0 {
		return nil
	}
	cfg := loadBlobSpillConfig()
	if !cfg.Enabled || len(data) <= cfg.Threshold {
		return nil
	}
	ref, err := writeBlobToStorage(cfg, kind, data, nameHint)
	if err != nil {
		logSpillError(cfg, kind, nameHint, len(data), err)
		return nil
	}
	if kind == "body" {
		AddBodyBlob(ctx, *ref)
	} else {
		AddRespBlob(ctx, *ref)
	}
	return ref
}

// writeBlobToStorage 把内容写入 storage.CreateFs(cfg.Key) 下的 blobPath，返回引用。
func writeBlobToStorage(cfg blobSpillConfig, kind string, data []byte, nameHint string) (*BlobRef, error) {
	fs, release, err := storage.CreateFs(cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("storage.CreateFs(%q): %w", cfg.Key, err)
	}
	defer release()

	sum := sha256.Sum256(data)
	relPath := blobPath(cfg.Prefix, kind, nameHint, sum)
	if dir := filepath.Dir(relPath); dir != "" && dir != "." {
		if err := storage.EnsureDir(fs, dir); err != nil {
			return nil, fmt.Errorf("EnsureDir(%q): %w", dir, err)
		}
	}
	if err := afero.WriteFile(fs, relPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("WriteFile(%q): %w", relPath, err)
	}
	return &BlobRef{
		Key:    cfg.Key,
		Path:   relPath,
		Size:   int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// blobPath 生成 "<prefix>/<yyyy>/<mm>/<dd>/<hint>-<kind>-<sha8>.json" 形式的相对路径。
// 路径相对 storage 根（CreateFs 的 key），可直接 fs.Open；sha8 保证同名不覆盖。
func blobPath(prefix, kind, nameHint string, sum [32]byte) string {
	hint := strings.TrimSpace(nameHint)
	if hint == "" {
		hint = "trace"
	}
	hint = storage.SanitizeFileName(strings.Trim(hint, "/"))
	if hint == "" {
		hint = "trace"
	}
	if len(hint) > 80 {
		hint = hint[:80]
	}
	now := time.Now()
	name := fmt.Sprintf("%s-%s-%s.json", hint, kind, hex.EncodeToString(sum[:4]))
	return filepath.ToSlash(filepath.Join(prefix, now.Format("2006"), now.Format("01"), now.Format("02"), name))
}

var spillLogState sync.Map // key -> time.Time（限频用）

// logSpillError 限频记录外溢失败（每分钟每 key 一条），避免存储不可用时刷爆日志。
func logSpillError(cfg blobSpillConfig, kind, nameHint string, size int, err error) {
	now := time.Now()
	if last, ok := spillLogState.Load(cfg.Key); ok {
		if t, ok := last.(time.Time); ok && now.Sub(t) < spillErrorInterval {
			return
		}
	}
	spillLogState.Store(cfg.Key, now)
	zap.L().Error("blob spill failed, keep content inline",
		zap.String("storageKey", cfg.Key),
		zap.String("prefix", cfg.Prefix),
		zap.String("kind", kind),
		zap.String("nameHint", nameHint),
		zap.Int("size", size),
		zap.Error(err),
	)
}
