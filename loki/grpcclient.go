package loki

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/grafana/loki/pkg/logproto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type GrpcClient struct {
	client logproto.PusherClient
	conn   *grpc.ClientConn
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

	client := logproto.NewPusherClient(conn)

	return &GrpcClient{
		client: client,
		conn:   conn,
	}, nil
}

func (c *GrpcClient) Close() error {
	return c.conn.Close()
}

func (c *GrpcClient) Push(labels map[string]string, line string) error {
	labelString := formatLabels(labels)

	req := &logproto.PushRequest{
		Streams: []logproto.Stream{
			{
				Labels: labelString,
				Entries: []logproto.Entry{
					{
						Timestamp: time.Now(),
						Line:      line,
					},
				},
			},
		},
	}

	var err error
	backoff := 100 * time.Millisecond
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err = c.client.Push(ctx, req)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	return err
}

func formatLabels(labels map[string]string) string {
	var kv []string
	for k, v := range labels {
		kv = append(kv, fmt.Sprintf("%s=%q", k, v))
	}
	sort.Strings(kv)
	return "{" + strings.Join(kv, ",") + "}"
}
