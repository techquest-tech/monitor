package loki

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsRetryableLokiError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"429 rate limit", errors.New("status=429 body=too many requests"), true},
		{"plain 429", errors.New("rpc error: code = Unavailable desc = 429"), true},
		{"too many requests text", errors.New("too many requests"), true},
		{"timeout substring", errors.New("read tcp: i/o timeout"), true},
		{"context deadline exceeded", errors.New("loki REST push failed: Post ...: context deadline exceeded"), true},
		{"grpc deadline exceeded", errors.New("rpc error: code = DeadlineExceeded desc = context deadline exceeded"), true},
		{"500 server error", errors.New("status=500 body=internal server error"), true},
		{"502 bad gateway", errors.New("status=502 body=bad gateway"), true},
		{"503 unavailable", errors.New("status=503 body=service unavailable"), true},
		{"504 gateway timeout", errors.New("status=504 body=gateway timeout"), true},
		{"temporarily unavailable", errors.New("temporarily unavailable"), true},
		{"connection reset", errors.New("connection reset by peer"), true},
		{"broken pipe", errors.New("write: broken pipe"), true},
		{"EOF", errors.New("EOF"), true},
		{"400 bad request not retryable", errors.New("status=400 body=invalid labels"), false},
		{"404 not found not retryable", errors.New("status=404 body=not found"), false},
		{"401 unauthorized not retryable", errors.New("status=401 body=unauthorized"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableLokiError(tc.err); got != tc.want {
				t.Fatalf("isRetryableLokiError(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestFormatLabelsDeterministic(t *testing.T) {
	a := formatLabels(map[string]string{"b": "2", "a": "1"})
	b := formatLabels(map[string]string{"a": "1", "b": "2"})
	if a != b {
		t.Fatalf("formatLabels not deterministic: %q vs %q", a, b)
	}
	if a != `{a="1",b="2"}` {
		t.Fatalf("unexpected formatLabels output: %q", a)
	}
}

func TestRestClientPushBatchGroupsByLabels(t *testing.T) {
	var gotBody []byte
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := NewRestClient(&LokiConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("NewRestClient failed: %v", err)
	}

	now := time.Now()
	err = c.PushBatch([]PushEntry{
		{Labels: map[string]string{"app": "a", "env": "x"}, Line: "line-1", Ts: now},
		{Labels: map[string]string{"app": "a", "env": "x"}, Line: "line-2", Ts: now},
		{Labels: map[string]string{"app": "b"}, Line: "line-3", Ts: now},
	})
	if err != nil {
		t.Fatalf("PushBatch failed: %v", err)
	}
	if gotPath != "/loki/api/v1/push" {
		t.Fatalf("unexpected path: %q", gotPath)
	}

	var body lokiJSONBody
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("response body is not valid loki push JSON: %v\nbody=%s", err, gotBody)
	}
	if len(body.Streams) != 2 {
		t.Fatalf("expected 2 streams, got %d: %s", len(body.Streams), gotBody)
	}
	// 两条同 label 的消息应聚合到同一个 stream 的 values 里。
	var appA, appB int
	for _, s := range body.Streams {
		switch s.Stream["app"] {
		case "a":
			appA = len(s.Values)
		case "b":
			appB = len(s.Values)
		}
	}
	if appA != 2 || appB != 1 {
		t.Fatalf("unexpected grouping: appA=%d appB=%d body=%s", appA, appB, gotBody)
	}
}

func TestRestClientPushBatchEmpty(t *testing.T) {
	c, err := NewRestClient(&LokiConfig{URL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewRestClient failed: %v", err)
	}
	if err := c.PushBatch(nil); err != nil {
		t.Fatalf("PushBatch(nil) should be a no-op, got %v", err)
	}
}

func TestSplitWithPrefixKeepsUTF8Boundaries(t *testing.T) {
	s := strings.Repeat("日志", 100) // 200 个 UTF-8 字符
	parts := splitWithPrefix(s, 30)
	joined := ""
	for _, p := range parts {
		joined += strings.TrimPrefix(strings.TrimPrefix(p, ""), "")
		// 每条分片必须能被 UTF-8 完整解码。
		if !strings.HasSuffix(p, "日志") && !strings.HasSuffix(p, "日") && !strings.HasSuffix(p, "志") {
			t.Fatalf("part not on utf8 boundary: %q", p)
		}
	}
	_ = joined
	if len(parts) < 2 {
		t.Fatalf("expected split, got %d parts", len(parts))
	}
}
