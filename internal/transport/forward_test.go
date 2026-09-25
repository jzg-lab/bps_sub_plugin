package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/config"
	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

// fakeStream 模拟宿主一侧：按顺序吐出请求帧，收集插件发回的响应帧。
type fakeStream struct {
	ctx    context.Context
	mu     sync.Mutex
	in     chan *pluginv1.ForwardRequest
	out    []*pluginv1.ForwardResponse
	sendFn func(*pluginv1.ForwardResponse) error
}

func newFakeStream(ctx context.Context, frames ...*pluginv1.ForwardRequest) *fakeStream {
	in := make(chan *pluginv1.ForwardRequest, len(frames))
	for _, frame := range frames {
		in <- frame
	}
	close(in)
	return &fakeStream{ctx: ctx, in: in}
}

func (s *fakeStream) Context() context.Context { return s.ctx }

func (s *fakeStream) Recv() (*pluginv1.ForwardRequest, error) {
	select {
	case frame, ok := <-s.in:
		if !ok {
			return nil, io.EOF
		}
		return frame, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func (s *fakeStream) Send(frame *pluginv1.ForwardResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendFn != nil {
		if err := s.sendFn(frame); err != nil {
			return err
		}
	}
	s.out = append(s.out, frame)
	return nil
}

func (s *fakeStream) frames() []*pluginv1.ForwardResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pluginv1.ForwardResponse(nil), s.out...)
}

func startFrame(start *pluginv1.ForwardRequestStart) *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: start}}
}

func chunkFrame(data string) *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: []byte(data)}}
}

func endFrame() *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}
}

func newForwarder(cfg config.Config) *Forwarder {
	return &Forwarder{Pool: NewPool(cfg), Stats: &Stats{}}
}

func directConfig() config.Config {
	cfg := config.Default()
	cfg.UseAccountProxy = false
	return cfg
}

// collect 把响应帧拆成 start、拼接后的 body、end 和 error。
func collect(t *testing.T, frames []*pluginv1.ForwardResponse) (*pluginv1.ForwardResponseStart, string, *pluginv1.ForwardResponseEnd, *pluginv1.ForwardResponseError) {
	t.Helper()
	var start *pluginv1.ForwardResponseStart
	var body bytes.Buffer
	var end *pluginv1.ForwardResponseEnd
	var frameErr *pluginv1.ForwardResponseError
	for i, frame := range frames {
		switch {
		case frame.GetStart() != nil:
			if i != 0 {
				t.Fatalf("start frame at position %d", i)
			}
			start = frame.GetStart()
		case frame.GetBodyChunk() != nil:
			body.Write(frame.GetBodyChunk())
		case frame.GetEnd() != nil:
			if i != len(frames)-1 {
				t.Fatalf("end frame is not last")
			}
			end = frame.GetEnd()
		case frame.GetError() != nil:
			if i != len(frames)-1 {
				t.Fatalf("error frame is not last")
			}
			frameErr = frame.GetError()
		}
	}
	return start, body.String(), end, frameErr
}

func TestForwardChunkedBodyWithKnownLength(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hello world" {
			t.Errorf("body = %q", body)
		}
		if r.ContentLength != int64(len("hello world")) || len(r.TransferEncoding) != 0 {
			t.Errorf("content length = %d, transfer encoding = %v", r.ContentLength, r.TransferEncoding)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" || r.URL.RawQuery != "a=1" {
			t.Errorf("unexpected request line: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Chatgpt-Account-Id") != "acc" {
			t.Errorf("identity headers not forwarded: %v", r.Header)
		}
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	defer upstream.Close()

	stream := newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{
			Method: http.MethodPost,
			Url:    upstream.URL + "/backend-api/codex/responses?a=1",
			Headers: map[string]*pluginv1.HeaderValues{
				"Authorization":      {Values: []string{"Bearer tok"}},
				"chatgpt-account-id": {Values: []string{"acc"}},
				"Content-Length":     {Values: []string{"999"}},
			},
			HasBody:       true,
			ContentLength: int64(len("hello world")),
		}),
		chunkFrame("hello "), chunkFrame("world"), endFrame(),
	)
	forwarder := newForwarder(directConfig())
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	start, body, end, frameErr := collect(t, stream.frames())
	if frameErr != nil {
		t.Fatalf("unexpected error frame: %+v", frameErr)
	}
	if start.StatusCode != http.StatusCreated || body != "created" || end.BytesReceived != int64(len("created")) {
		t.Fatalf("start=%+v body=%q end=%+v", start, body, end)
	}
	if got := start.Headers["X-Multi"].GetValues(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("repeated headers = %v", got)
	}
	if forwarder.Stats.Total.Load() != 1 || forwarder.Stats.Failed.Load() != 0 || forwarder.Stats.InFlight.Load() != 0 {
		t.Fatalf("stats = total %d failed %d inflight %d", forwarder.Stats.Total.Load(), forwarder.Stats.Failed.Load(), forwarder.Stats.InFlight.Load())
	}
}

func TestForwardUnknownLengthUsesChunkedEncoding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "abc" || r.ContentLength != -1 {
			t.Errorf("body=%q content length=%d", body, r.ContentLength)
		}
	}))
	defer upstream.Close()

	stream := newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{Method: http.MethodPost, Url: upstream.URL, HasBody: true, ContentLength: -1}),
		chunkFrame("a"), chunkFrame("bc"), endFrame(),
	)
	if err := newForwarder(directConfig()).Forward(stream); err != nil {
		t.Fatal(err)
	}
	if _, _, _, frameErr := collect(t, stream.frames()); frameErr != nil {
		t.Fatalf("unexpected error frame: %+v", frameErr)
	}
}

func TestForwardWithoutBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 {
			t.Errorf("content length = %d", r.ContentLength)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	stream := newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{Method: http.MethodGet, Url: upstream.URL, Host: "override.example"}),
		endFrame(),
	)
	if err := newForwarder(directConfig()).Forward(stream); err != nil {
		t.Fatal(err)
	}
	start, body, _, frameErr := collect(t, stream.frames())
	if frameErr != nil || start.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("start=%+v body=%q err=%+v", start, body, frameErr)
	}
}

func TestForwardUsesHostOverride(t *testing.T) {
	var gotHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
	}))
	defer upstream.Close()

	stream := newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{Method: http.MethodGet, Url: upstream.URL, Host: "chatgpt.com"}),
		endFrame(),
	)
	if err := newForwarder(directConfig()).Forward(stream); err != nil {
		t.Fatal(err)
	}
	if gotHost != "chatgpt.com" {
		t.Fatalf("host = %q", gotHost)
	}
}

func TestForwardStreamsResponseIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: second\n\n"))
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	firstChunk := make(chan struct{})
	var once sync.Once
	stream := newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{Method: http.MethodPost, Url: upstream.URL}),
		endFrame(),
	)
	stream.sendFn = func(frame *pluginv1.ForwardResponse) error {
		if strings.Contains(string(frame.GetBodyChunk()), "first") {
			once.Do(func() { close(firstChunk) })
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- newForwarder(directConfig()).Forward(stream) }()

	select {
	case <-firstChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("first SSE event was not forwarded before upstream finished")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, body, end, _ := collect(t, stream.frames())
	if body != "data: first\n\ndata: second\n\n" || end == nil {
		t.Fatalf("body=%q end=%+v", body, end)
	}
}

func TestForwardConnectFailureReportsNotSent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	stream := newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{Method: http.MethodPost, Url: "http://" + address, HasBody: true, ContentLength: 1}),
		chunkFrame("x"), endFrame(),
	)
	forwarder := newForwarder(directConfig())
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	_, _, _, frameErr := collect(t, stream.frames())
	if frameErr == nil || frameErr.Code != codeUpstreamFailed || frameErr.RequestSent {
		t.Fatalf("error frame = %+v", frameErr)
	}
	if forwarder.Stats.Failed.Load() != 1 {
		t.Fatalf("failed = %d", forwarder.Stats.Failed.Load())
	}
}

func TestForwardResponseReadFailureReportsSent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer upstream.Close()

	stream := newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{Method: http.MethodGet, Url: upstream.URL}),
		endFrame(),
	)
	if err := newForwarder(directConfig()).Forward(stream); err != nil {
		t.Fatal(err)
	}
	start, _, _, frameErr := collect(t, stream.frames())
	if start == nil || frameErr == nil || frameErr.Code != codeUpstreamBody || !frameErr.RequestSent {
		t.Fatalf("start=%+v error=%+v", start, frameErr)
	}
}

func TestForwardRejectsInvalidStart(t *testing.T) {
	cases := map[string][]*pluginv1.ForwardRequest{
		"missing start":  {chunkFrame("x")},
		"missing method": {startFrame(&pluginv1.ForwardRequestStart{Url: "https://example.com"})},
		"bad scheme":     {startFrame(&pluginv1.ForwardRequestStart{Method: "GET", Url: "ftp://example.com"})},
		"no host":        {startFrame(&pluginv1.ForwardRequestStart{Method: "GET", Url: "/relative"})},
	}
	for name, frames := range cases {
		stream := newFakeStream(context.Background(), frames...)
		if err := newForwarder(directConfig()).Forward(stream); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, _, _, frameErr := collect(t, stream.frames())
		if frameErr == nil || frameErr.Code != codeInvalidRequest || frameErr.RequestSent {
			t.Fatalf("%s: error frame = %+v", name, frameErr)
		}
	}
}

func TestForwardCancelledContextStopsUpstream(t *testing.T) {
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeStream(ctx,
		startFrame(&pluginv1.ForwardRequestStart{Method: http.MethodGet, Url: upstream.URL}),
		endFrame(),
	)
	stream.sendFn = func(frame *pluginv1.ForwardResponse) error {
		if frame.GetStart() != nil {
			cancel()
		}
		return nil
	}
	done := make(chan error, 1)
	forwarder := newForwarder(directConfig())
	go func() { done <- forwarder.Forward(stream) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Forward did not return after context cancellation")
	}
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was not cancelled")
	}
	// 宿主读完 response.completed 就会关闭流，这不算失败。
	if forwarder.Stats.Failed.Load() != 0 || forwarder.Stats.Cancelled.Load() != 1 {
		t.Fatalf("failed=%d cancelled=%d", forwarder.Stats.Failed.Load(), forwarder.Stats.Cancelled.Load())
	}
}

func TestForwardSendFailureIsReturned(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	stream := newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{Method: http.MethodGet, Url: upstream.URL}),
		endFrame(),
	)
	sendErr := errors.New("host gone")
	stream.sendFn = func(*pluginv1.ForwardResponse) error { return sendErr }
	forwarder := newForwarder(directConfig())
	if err := forwarder.Forward(stream); !errors.Is(err, sendErr) {
		t.Fatalf("err = %v", err)
	}
	if forwarder.Stats.Failed.Load() != 1 {
		t.Fatalf("failed = %d", forwarder.Stats.Failed.Load())
	}
}

func TestPoolProxySelection(t *testing.T) {
	cfg := config.Default()
	pool := NewPool(cfg)

	direct, err := pool.Get("")
	if err != nil || direct.Proxy != nil {
		t.Fatalf("direct transport: %v proxy=%v", err, direct.Proxy != nil)
	}
	proxied, err := pool.Get("socks5h://127.0.0.1:1080")
	if err != nil || proxied.Proxy == nil {
		t.Fatalf("proxied transport: %v", err)
	}
	again, _ := pool.Get("socks5h://127.0.0.1:1080")
	if again != proxied {
		t.Fatal("transport for the same proxy should be reused")
	}
	if _, err := pool.Get("ftp://127.0.0.1"); err == nil {
		t.Fatal("unsupported proxy scheme should fail")
	}

	cfg.UseAccountProxy = false
	pool.Apply(cfg)
	ignored, err := pool.Get("socks5h://127.0.0.1:1080")
	if err != nil || ignored.Proxy != nil || ignored == proxied {
		t.Fatalf("proxy should be ignored after disabling account proxy: %v", err)
	}
}

func TestPoolHTTP2Toggle(t *testing.T) {
	cfg := config.Default()
	cfg.EnableHTTP2 = false
	transport, _ := NewPool(cfg).Get("")
	if transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil {
		t.Fatal("HTTP/2 should be disabled")
	}
	transport, _ = NewPool(config.Default()).Get("")
	if !transport.ForceAttemptHTTP2 || transport.TLSNextProto != nil {
		t.Fatal("HTTP/2 should be enabled by default")
	}
}

func TestPoolEvictsOldestProxy(t *testing.T) {
	pool := NewPool(config.Default())
	for i := 0; i < maxPooledProxies+5; i++ {
		if _, err := pool.Get(fmt.Sprintf("http://127.0.0.1:%d", 10000+i)); err != nil {
			t.Fatal(err)
		}
	}
	pool.mu.Lock()
	size := len(pool.entries)
	pool.mu.Unlock()
	if size > maxPooledProxies {
		t.Fatalf("pool size %d exceeds limit %d", size, maxPooledProxies)
	}
}
