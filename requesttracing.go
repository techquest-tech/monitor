package monitor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/carlmjohnson/requests"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"go.uber.org/zap"
)

func LogOutbound(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return requests.RoundTripFunc(func(req *http.Request) (res *http.Response, err error) {
		start := time.Now()
		fullLogging := TracingDetails{
			Method:    req.Method,
			UserAgent: req.UserAgent(),
			StartedAt: time.Now(),
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
		logger := zap.L().With(zap.String("Optionname", fullLogging.Optionname))

		logger.Info("outbound request")
		if req.Body != nil {
			// reqcache := make([]byte, 1024)
			reqcache, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewBuffer(reqcache))
			reqbody := string(reqcache)
			ct := req.Header.Get("Content-Type")
			if req.Method == "POST" && ct == "application/x-www-form-urlencoded" {
				reqbody, _ = url.QueryUnescape(reqbody)
			}
			fullLogging.Body = reqbody
		}

		res, err = rt.RoundTrip(req)

		dur := time.Since(start)
		fullLogging.Durtion = dur
		if err != nil {
			wrapError := fmt.Errorf("requst to %s, resp err %v", fullLogging.Uri, err)
			// core.Bus.Publish(core.EventError, wrapError)
			fullLogging.Resp = "error:" + err.Error()
			core.ErrorAdaptor.Push(core.ErrorReport{
				Error: wrapError,
				Uri:   fullLogging.Uri,
			})
			logger.Error("outbound request error", zap.Error(wrapError))
		} else {
			respdetails, err := DumpRespBody(res)
			if err == nil && len(respdetails) > 0 {
				fullLogging.Resp = string(respdetails)
			}
			if err != nil || res.StatusCode >= 400 {
				rr := core.ErrorReport{
					Error:     err,
					Uri:       fullLogging.Uri,
					FullStack: respdetails,
					HappendAT: time.Now(),
				}
				if rr.Error == nil {
					rr.Error = fmt.Errorf("res unexpected status code %d", res.StatusCode)
				}
				core.ErrorAdaptor.Push(rr)
			}
			fullLogging.Status = res.StatusCode
			logger.Info("outbound request done", zap.Int("status", res.StatusCode), zap.Duration("duration", dur))
		}

		// core.Bus.Publish(core.EventTracing, fullLogging)
		TracingAdaptor.Push(fullLogging)
		return
	})
}

// emptyBody is an instance of empty reader.
var emptyBody = io.NopCloser(strings.NewReader(""))

func drainBody(b io.ReadCloser) (r1, r2 io.ReadCloser, err error) {
	if b == nil || b == http.NoBody {
		// No copying needed. Preserve the magic sentinel meaning of NoBody.
		return http.NoBody, http.NoBody, nil
	}
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(b); err != nil {
		return nil, b, err
	}
	if err = b.Close(); err != nil {
		return nil, b, err
	}
	return io.NopCloser(&buf), io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

func DumpRespBody(resp *http.Response) ([]byte, error) {
	var b bytes.Buffer
	var err error
	save := resp.Body
	savecl := resp.ContentLength

	if resp.Body == nil || resp.ContentLength == 0 {
		resp.Body = emptyBody
	} else {
		save, resp.Body, err = drainBody(resp.Body)
		if err != nil {
			return nil, err
		}
	}

	_, err = io.Copy(&b, resp.Body)
	if err != nil {
		return nil, err
	}

	resp.Body = save
	resp.ContentLength = savecl
	if err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
