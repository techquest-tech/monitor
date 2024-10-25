package monitor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/carlmjohnson/requests"
	"github.com/techquest-tech/gin-shared/pkg/core"
)

func LogOutbound(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return requests.RoundTripFunc(func(req *http.Request) (res *http.Response, err error) {
		start := time.Now()
		fullLogging := &TracingDetails{
			Method:    req.Method,
			UserAgent: req.UserAgent(),
		}
		uri := req.RequestURI
		if uri == "" {
			uri = req.URL.String()
		}
		fullLogging.Uri = uri
		if index := strings.Index(uri, "?"); index >= 0 {
			uri = uri[:index]
		}
		fullLogging.Optionname = fmt.Sprintf("[%s]%s", req.Method, uri)

		if req.Body != nil {
			// reqcache := make([]byte, 1024)
			reqcache, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewBuffer(reqcache))
			fullLogging.Body = string(reqcache)
		}

		res, err = rt.RoundTrip(req)

		dur := time.Since(start)
		fullLogging.Durtion = dur
		if err != nil {
			wrapError := fmt.Errorf("requst to %s, resp err %v", fullLogging.Uri, err)
			core.Bus.Publish(core.EventError, wrapError)
			fullLogging.Resp = "error:" + err.Error()
		} else {
			respdetails, err := httputil.DumpResponse(res, true)
			if err == nil && len(respdetails) > 0 {
				fullLogging.Resp = string(respdetails)
			}
			fullLogging.Status = res.StatusCode
		}

		core.Bus.Publish(core.EventTracing, fullLogging)
		return
	})
}
