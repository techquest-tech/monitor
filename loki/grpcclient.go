package loki

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/grafana/loki/pkg/push"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type GrpcClient struct {
	client  push.PusherClient
	conn    *grpc.ClientConn
	timeout time.Duration
}

// BasicAuthCreds implements credentials.PerRPCCredentials for Basic Auth
type BasicAuthCreds struct {
	User     string
	Password string
	TLS      bool
}

func (c *BasicAuthCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	auth := c.User + ":" + c.Password
	enc := base64.StdEncoding.EncodeToString([]byte(auth))
	return map[string]string{
		"authorization": "Basic " + enc,
	}, nil
}

func (c *BasicAuthCreds) RequireTransportSecurity() bool {
	return c.TLS
}

func NewGrpcClient(conf *LokiConfig) (*GrpcClient, error) {
	// Strip http:// or https:// if present
	address := conf.URL
	address = strings.TrimPrefix(address, "http://")
	address = strings.TrimPrefix(address, "https://")

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	if conf.User != "" || conf.Password != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(&BasicAuthCreds{
			User:     conf.User,
			Password: conf.Password,
			TLS:      false, // Assume insecure for now as per previous config
		}))
	}

	conn, err := grpc.NewClient(address, opts...)
	if err != nil {
		return nil, err
	}

	client := push.NewPusherClient(conn)

	return &GrpcClient{
		client:  client,
		conn:    conn,
		timeout: 5 * time.Second,
	}, nil
}

func (c *GrpcClient) Close() error {
	return c.conn.Close()
}

// PushBatch writes one or more log lines in a single gRPC push request. Retry
// is intentionally left to the caller (LokiSetting.pushBatch) so there is a
// single, consistent retry/backoff policy for both REST and gRPC clients.
func (c *GrpcClient) PushBatch(entries []PushEntry) error {
	if len(entries) == 0 {
		return nil
	}

	index := make(map[string]int, 1)
	streams := make([]push.Stream, 0, 1)
	for _, e := range entries {
		key := formatLabels(e.Labels)
		i, ok := index[key]
		if !ok {
			i = len(streams)
			index[key] = i
			streams = append(streams, push.Stream{Labels: key})
		}
		streams[i].Entries = append(streams[i].Entries, push.Entry{
			Timestamp: e.Ts,
			Line:      e.Line,
		})
	}

	req := &push.PushRequest{Streams: streams}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	_, err := c.client.Push(ctx, req)
	return err
}

// formatLabels renders a label set as a deterministic, sorted Loki label string
// (e.g. {app="x",env="y"}). It doubles as the gRPC Labels field and as a stable
// grouping key for batch entries sharing the same label set.
func formatLabels(labels map[string]string) string {
	var kv []string
	for k, v := range labels {
		kv = append(kv, fmt.Sprintf("%s=%q", k, v))
	}
	sort.Strings(kv)
	return "{" + strings.Join(kv, ",") + "}"
}
