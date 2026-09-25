package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
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
	codeRequestBody      = "BPS_REQUEST_BODY_FAILED"
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
	// Cancelled 统计宿主主动结束的流：宿主读到 response.completed 后会直接关闭流，
	// 这是正常行为，不算失败。
	Cancelled atomic.Int64

	// RoutedBPS / RoutedCodex 是最终发往哪个上游的计数（回落算 codex）。
	RoutedBPS   atomic.Int64
	RoutedCodex atomic.Int64
	// SkipReasons 是 Responses 请求没有改走 basispoints 的原因。
	SkipReasons Counter
	// Fallbacks 是 basispoints 失败后回落 codex 的原因。
	Fallbacks Counter
	// BPSStatus 是 basispoints 返回的 HTTP 状态码（连接失败记为 "error"）。
	BPSStatus Counter
}

// Counter 是按字符串键计数的并发安全映射。
type Counter struct {
	mu     sync.Mutex
	values map[string]int64
}

// Add 给 key 加一。
func (c *Counter) Add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = make(map[string]int64)
	}
	c.values[key]++
}

// Get 返回 key 的当前计数。
func (c *Counter) Get(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key]
}

// Snapshot 返回当前计数的副本。
func (c *Counter) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.values))
	for key, value := range c.values {
		out[key] = value
	}
	return out
}

// Forwarder 把宿主的请求帧还原为 HTTP 请求，按配置改走 basispoints 或原样发往 codex，
// 再把原始响应按帧回传。
type Forwarder struct {
	Pool  *Pool
	Stats *Stats

	// roundTripper 非空时替代连接池，仅供测试把 chatgpt.com 指向本地服务器。
	roundTripper http.RoundTripper
}

// Forward 处理一次完整的转发流。协议错误通过 error 帧告知宿主，返回值只表示流本身出错。
func (f *Forwarder) Forward(stream Stream) error {
	f.Stats.Total.Add(1)
	f.Stats.InFlight.Add(1)
	defer f.Stats.InFlight.Add(-1)
	ctx := stream.Context()
	err := f.forward(ctx, stream)
	switch {
	case ctx.Err() != nil:
		f.Stats.Cancelled.Add(1)
		return ctx.Err()
	case errors.Is(err, errFrameSent):
		f.Stats.Failed.Add(1)
		return nil
	case err != nil:
		f.Stats.Failed.Add(1)
	}
	return err
}

// errFrameSent 表示已经用 error 帧告知宿主失败，流本身正常结束。
var errFrameSent = errors.New("error frame sent")

func (f *Forwarder) forward(ctx context.Context, stream Stream) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return f.fail(stream, codeInvalidRequest, "首帧必须是 start", false)
	}
	request, err := buildRequest(ctx, start)
	if err != nil {
		return f.fail(stream, codeInvalidRequest, err.Error(), false)
	}

	bodyReader, bodyWriter := io.Pipe()
	defer func() { _ = bodyReader.Close() }()
	go pumpRequestBody(stream, bodyWriter)
	if !start.HasBody {
		// 仍需消费 body_end 帧，读端关闭后 pump 会丢弃后续数据。
		_ = bodyReader.Close()
	}

	var transport http.RoundTripper = f.roundTripper
	if transport == nil {
		pooled, err := f.Pool.Get(start.ProxyUrl)
		if err != nil {
			return f.fail(stream, codeTransportUnready, err.Error(), false)
		}
		transport = pooled
	}
	cfg := f.Pool.Config()

	if start.HasBody && cfg.BPSEnabled && isResponsesCandidate(request) {
		return f.forwardResponses(ctx, stream, transport, cfg, request, bodyReader, start.ContentLength)
	}

	if start.HasBody {
		setStreamingBody(request, bodyReader, start.ContentLength)
	} else {
		request.Body = http.NoBody
		request.ContentLength = 0
	}
	f.Stats.RoutedCodex.Add(1)
	return f.sendAndRelay(ctx, stream, transport, request, false)
}

// setStreamingBody 把请求体设为从宿主流式读取。
func setStreamingBody(request *http.Request, body io.Reader, contentLength int64) {
	request.Body = io.NopCloser(body)
	request.ContentLength = contentLength
	// net/http 把 0 长度加非空 Body 同样视为长度未知，统一用 -1 走 chunked。
	if request.ContentLength <= 0 {
		request.ContentLength = -1
	}
}

// sendAndRelay 发出请求并把响应原样回传。priorSent 表示之前的尝试可能已被上游处理，
// 失败时必须上报 request_sent=true 以阻止宿主重放。
func (f *Forwarder) sendAndRelay(ctx context.Context, stream Stream, transport http.RoundTripper, request *http.Request, priorSent bool) error {
	started := time.Now()
	response, sent, err := roundTrip(ctx, transport, request)
	if err != nil {
		// 只有确认请求头从未写出时才允许宿主换账号重放。
		return f.fail(stream, codeUpstreamFailed, err.Error(), sent || priorSent)
	}
	return f.relay(stream, response, started)
}

// roundTrip 发出请求，并报告请求头是否已经写出（写出后上游可能已开始处理）。
func roundTrip(ctx context.Context, transport http.RoundTripper, request *http.Request) (*http.Response, bool, error) {
	var headersWritten atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { headersWritten.Store(true) }}
	request = request.WithContext(httptrace.WithClientTrace(ctx, trace))
	response, err := transport.RoundTrip(request)
	return response, headersWritten.Load(), err
}

// relay 把上游响应按帧回传宿主，并负责关闭响应体。
func (f *Forwarder) relay(stream Stream, response *http.Response, started time.Time) error {
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

// fail 用 error 帧告知宿主失败。发送成功返回 errFrameSent，否则返回发送错误。
func (f *Forwarder) fail(stream Stream, code, message string, requestSent bool) error {
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
		Code:        code,
		Message:     message,
		RequestSent: requestSent,
	}}}); err != nil {
		return err
	}
	return errFrameSent
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
