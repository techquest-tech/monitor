package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// TestBlobPath 覆盖路径生成约定：前缀 + 年/月/日目录 + "<hint>-<kind>-<sha8>.json"。
func TestBlobPath(t *testing.T) {
	sum := sha256.Sum256([]byte("hello"))
	got := blobPath("traceblob", "resp", "/v1/xxy/qcquery/today", sum)

	if !strings.HasPrefix(got, "traceblob/") {
		t.Fatalf("缺少 prefix: %s", got)
	}
	if strings.Contains(got, "//") {
		t.Fatalf("出现空目录段: %s", got)
	}
	if want := "-resp-" + hex.EncodeToString(sum[:4]) + ".json"; !strings.HasSuffix(got, want) {
		t.Fatalf("文件名后缀不符 %q: %s", want, got)
	}
	if base := filepath.Base(got); base == "" || base == "." || strings.Contains(base, "/") {
		t.Fatalf("非法文件名: %q", base)
	}
}

// TestBlobPathHintSanitized hint 为空/纯分隔符/超长时都必须生成合法文件名（不越界、不带路径分隔符）。
func TestBlobPathHintSanitized(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	for _, hint := range []string{"", "   ", "////", strings.Repeat("a", 300)} {
		got := blobPath("traceblob", "body", hint, sum)
		base := filepath.Base(got)
		if base == "" || base == "." || base == "/" || strings.Contains(base, "/") {
			t.Fatalf("hint=%q 生成非法文件名: %q", hint, got)
		}
		if len(base) > 200 {
			t.Fatalf("hint=%q 文件名过长(%d): %s", hint, len(base), base)
		}
	}
}

// TestLoadBlobSpillConfigDefaults 缺省值必须就是需求：启用 + 128 KiB + fileroot + traceblob。
func TestLoadBlobSpillConfigDefaults(t *testing.T) {
	viper.Reset()
	cfg := loadBlobSpillConfig()
	if !cfg.Enabled {
		t.Fatalf("缺省应启用: %+v", cfg)
	}
	if cfg.Threshold != 128*1024 {
		t.Fatalf("缺省阈值应为 128 KiB，实际 %d", cfg.Threshold)
	}
	if cfg.Key != "fileroot" || cfg.Prefix != "traceblob" {
		t.Fatalf("缺省 storage key/prefix 不符: %+v", cfg)
	}
}

// TestLoadBlobSpillConfigOverride 运行期覆盖（viper.Set）要生效。
func TestLoadBlobSpillConfigOverride(t *testing.T) {
	viper.Reset()
	viper.Set("tracing.blob.enabled", false)
	viper.Set("tracing.blob.threshold", 4096)
	viper.Set("tracing.blob.key", "traceblob")
	viper.Set("tracing.blob.prefix", "/blobs/")
	cfg := loadBlobSpillConfig()
	if cfg.Enabled || cfg.Threshold != 4096 || cfg.Key != "traceblob" || cfg.Prefix != "blobs/" {
		t.Fatalf("覆盖未生效: %+v", cfg)
	}
}

// TestSpillIfLargeWritesToStorage 超阈值必须落盘且引用字段正确（用本地 storage 兜底后端验证）。
func TestSpillIfLargeWritesToStorage(t *testing.T) {
	viper.Reset()
	dir := t.TempDir()
	viper.Set("tracing.blob.key", "testblob")
	viper.Set("testblob.path", dir) // type 未注册 → storage 回落本地文件系统
	viper.Set("tracing.blob.threshold", 8)

	data := []byte("0123456789abcdef")
	ref := SpillIfLarge(context.Background(), "resp", data, "op")
	if ref == nil {
		t.Fatal("超过阈值应返回引用")
	}
	if ref.Key != "testblob" {
		t.Fatalf("Key 不符: %s", ref.Key)
	}
	if ref.Size != int64(len(data)) {
		t.Fatalf("Size 不符: %d", ref.Size)
	}
	sum := sha256.Sum256(data)
	if ref.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256 不符: %s", ref.SHA256)
	}
	got, err := os.ReadFile(filepath.Join(dir, ref.Path))
	if err != nil {
		t.Fatalf("文件未落盘: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("落盘内容不一致: %q", string(got))
	}
}

// TestSpillIfLargeSkipsSmall 未超阈值不得落盘、不得返回引用。
func TestSpillIfLargeSkipsSmall(t *testing.T) {
	viper.Reset()
	dir := t.TempDir()
	viper.Set("tracing.blob.key", "testblobs")
	viper.Set("testblobs.path", dir)
	viper.Set("tracing.blob.threshold", 1024)

	if ref := SpillIfLarge(context.Background(), "body", []byte("small"), "op"); ref != nil {
		t.Fatalf("未超阈值不应外溢: %+v", ref)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("未超阈值不应写文件，实际 %d 个", len(entries))
	}
}

// TestSpillIfLargeDisabled 关闭开关后即使超阈值也不落盘。
func TestSpillIfLargeDisabled(t *testing.T) {
	viper.Reset()
	dir := t.TempDir()
	viper.Set("tracing.blob.enabled", false)
	viper.Set("tracing.blob.key", "testblobd")
	viper.Set("testblobd.path", dir)
	viper.Set("tracing.blob.threshold", 1)

	if ref := SpillIfLarge(context.Background(), "resp", []byte("x"), "op"); ref != nil {
		t.Fatalf("开关关闭不应外溢: %+v", ref)
	}
}
