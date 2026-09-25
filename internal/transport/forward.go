package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

// 错误帧里的 code，宿主只做展示和日志。
const (
	codeInvalidRequest   = "BPS_INVALID_REQUEST"
	codeTransportUnready = "BPS_TRANSPORT_UNAVAILABLE"
	codeUpstreamFailed   = "BPS_UPSTREAM_REQUEST_FAILED"
	codeUpstreamBody     = "BPS_UPSTREAM_BODY_FAILED"
)

const responseChunkSize = 32 * 1024

// Stream 是 Forward 双向流的最小接口，gRPC 生成的服务端流天然满足，测试可自行实现。
type Stream interface {
	Context() context.Context
	Send(*pluginv1.ForwardResponse) error
	Recv() (*pluginv1.ForwardRequest, error)
}

// Stats 是只读运行时计数，供 Health 的 status_json 展示。
type Stats struct {
	Total    atomic.Int64
	InFlight atomic.Int64
	Failed   atomic.Int64
}

// Forwarder 把宿主的请求帧还原为 HTTP 请求发往上游，再把原始响应按帧回传。
// 阶段 1 只做原样透传。
type Forwarder struct {
	Pool  *Pool
	Stats *Stats
}

// Forward 处理一次完整的转发流。协议错误通过 error 帧告知宿主，返回值只表示流本身出错。
func (f *Forwarder) Forward(stream Stream) error {
	f.Stats.Total.Add(1)
	f.Stats.InFlight.Add(1)
	defer f.Stats.InFlight.Add(-1)

	first, err := stream.Recv()
	if err != nil {
		f.Stats.Failed.Add(1)
		return err
	}
	start := first.GetStart()
	if start == nil {
		return f.fail(stream, codeInvalidRequest, "首帧必须是 start", false)
	}
	ctx := stream.Context()
	request, err := buildRequest(ctx, start)
	if err != nil {
		return f.fail(stream, codeInvalidRequest, err.Error(), false)
	}

	bodyReader, bodyWriter := io.Pipe()
	go pumpRequestBody(stream, bodyWriter)
	if start.HasBody {
		request.Body = bodyReader
		request.ContentLength = start.ContentLength
		// net/http 把 0 长度加非空 Body 同样视为长度未知，统一用 -1 走 chunked。
		if request.ContentLength <= 0 {
			request.ContentLength = -1
		}
	} else {
		request.Body = http.NoBody
		request.ContentLength = 0
		// 仍需消费 body_end 帧，读端关闭后 pump 会丢弃后续数据。
		_ = bodyReader.Close()
	}

	transport, err := f.Pool.Get(start.ProxyUrl)
	if err != nil {
		_ = bodyReader.Close()
		return f.fail(stream, codeTransportUnready, err.Error(), false)
	}

	var headersWritten atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { headersWritten.Store(true) }}
	request = request.WithContext(httptrace.WithClientTrace(ctx, trace))

	started := time.Now()
	response, err := transport.RoundTrip(request)
	if err != nil {
		_ = bodyReader.Close()
		// 只有确认请求头从未写出时才允许宿主换账号重放。
		return f.fail(stream, codeUpstreamFailed, err.Error(), headersWritten.Load())
	}
	defer func() { _ = response.Body.Close() }()

	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode:    int32(response.StatusCode),
		Status:        response.Status,
		Protocol:      response.Proto,
		ProtocolMajor: int32(response.ProtoMajor),
		ProtocolMinor: int32(response.ProtoMinor),
		Headers:       headersToPlugin(response.Header),
		ContentLength: response.ContentLength,
	}}}); err != nil {
		f.Stats.Failed.Add(1)
		return err
	}

	var received int64
	buffer := make([]byte, responseChunkSize)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			received += int64(n)
			chunk := append([]byte(nil), buffer[:n]...)
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: chunk}}); err != nil {
				f.Stats.Failed.Add(1)
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return f.fail(stream, codeUpstreamBody, readErr.Error(), true)
		}
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{
		BytesReceived: received,
		DurationMs:    time.Since(started).Milliseconds(),
	}}})
}

func (f *Forwarder) fail(stream Stream, code, message string, requestSent bool) error {
	f.Stats.Failed.Add(1)
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
		Code:        code,
		Message:     message,
		RequestSent: requestSent,
	}}})
}

// buildRequest 按 start 帧还原上游请求，但不设置请求体。
func buildRequest(ctx context.Context, start *pluginv1.ForwardRequestStart) (*http.Request, error) {
	if start.Method == "" {
		return nil, errors.New("缺少 method")
	}
	target, err := url.Parse(start.Url)
	if err != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return nil, errors.New("上游地址无效")
	}
	request, err := http.NewRequestWithContext(ctx, start.Method, target.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header = headersFromPlugin(start.Headers)
	// 长度与分块由 net/http 根据 ContentLength 重新生成，不能沿用宿主的值。
	for _, key := range []string{"Content-Length", "Transfer-Encoding", "Host", "Connection"} {
		request.Header.Del(key)
	}
	if host := strings.TrimSpace(start.Host); host != "" {
		request.Host = host
	}
	return request, nil
}

// pumpRequestBody 把 body_chunk 帧写入管道，直到 body_end、流结束或读端关闭。
func pumpRequestBody(stream Stream, writer *io.PipeWriter) {
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			_ = writer.Close()
			return
		}
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			// 读端关闭后（请求已完成或失败）写入会失败，忽略并继续消费剩余帧直到流结束。
			_, _ = writer.Write(chunk)
			continue
		}
		if frame.GetBodyEnd() {
			_ = writer.Close()
		}
	}
}

func headersToPlugin(headers http.Header) map[string]*pluginv1.HeaderValues {
	out := make(map[string]*pluginv1.HeaderValues, len(headers))
	for key, values := range headers {
		out[key] = &pluginv1.HeaderValues{Values: append([]string(nil), values...)}
	}
	return out
}

func headersFromPlugin(headers map[string]*pluginv1.HeaderValues) http.Header {
	out := make(http.Header, len(headers))
	for key, values := range headers {
		if values != nil {
			out[http.CanonicalHeaderKey(key)] = append([]string(nil), values.Values...)
		}
	}
	return out
}
