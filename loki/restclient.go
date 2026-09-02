package loki

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/carlmjohnson/requests"
)

type RestClient struct {
	endpoint string
	auth     string
	timeout  time.Duration
}

func NewRestClient(conf *LokiConfig) (*RestClient, error) {
	url := strings.TrimRight(conf.URL, "/") + "/loki/api/v1/push"
	var auth string
	if conf.User != "" || conf.Password != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(conf.User + ":" + conf.Password))
		auth = "Basic " + cred
	}
	return &RestClient{
		endpoint: url,
		auth:     auth,
		timeout:  5 * time.Second,
	}, nil
}

func (c *RestClient) Close() error { return nil }

type lokiJSONStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}
type lokiJSONBody struct {
	Streams []lokiJSONStream `json:"streams"`
}

// PushBatch writes one or more log lines in a single HTTP request. Entries that
// share the same label set are grouped into one stream so the payload stays
// compact and the request count (the thing that trips Loki rate limits) is
// minimized.
func (c *RestClient) PushBatch(entries []PushEntry) error {
	if len(entries) == 0 {
		return nil
	}

	index := make(map[string]int, 1)
	streams := make([]lokiJSONStream, 0, 1)
	for _, e := range entries {
		key := formatLabels(e.Labels)
		i, ok := index[key]
		if !ok {
			i = len(streams)
			index[key] = i
			streams = append(streams, lokiJSONStream{Stream: e.Labels})
		}
		streams[i].Values = append(streams[i].Values, []string{
			strconv.FormatInt(e.Ts.UnixNano(), 10),
			e.Line,
		})
	}

	body := lokiJSONBody{Streams: streams}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	var status int
	var respBody []byte
	req := requests.
		URL(c.endpoint).
		Method("POST").
		BodyJSON(body).
		Header("Content-Type", "application/json")
	if c.auth != "" {
		req = req.Header("Authorization", c.auth)
	}

	err := req.Handle(func(r *http.Response) error {
		status = r.StatusCode
		b, rerr := io.ReadAll(r.Body)
		if rerr != nil {
			return rerr
		}
		respBody = b
		if status/100 != 2 {
			msg := strings.TrimSpace(string(respBody))
			if msg == "" {
				msg = http.StatusText(status)
			}
			return fmt.Errorf("status=%d body=%s", status, msg)
		}
		return nil
	}).Fetch(ctx)
	if err != nil {
		return fmt.Errorf("loki REST push failed: %w", err)
	}
	return nil
}
