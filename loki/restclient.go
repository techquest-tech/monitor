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

func (c *RestClient) Push(labels map[string]string, line string) error {
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	body := lokiJSONBody{
		Streams: []lokiJSONStream{
			{
				Stream: labels,
				Values: [][]string{
					{ts, line},
				},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
