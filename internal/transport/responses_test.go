package transport

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jzg-lab/bps_sub_plugin/internal/config"
	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

const codexURL = "https://chatgpt.com/backend-api/codex/responses"

// redirectTransport 把发往 chatgpt.com 的请求改发到本地 codex 测试服务器，其他请求照常。
type redirectTransport struct {
	codex *url.URL
}

func (r redirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Hostname() == "chatgpt.com" {
		clone := request.Clone(request.Context())
		clone.URL.Scheme = r.codex.Scheme
		clone.URL.Host = r.codex.Host
		return http.DefaultTransport.RoundTrip(clone)
	}
	return http.DefaultTransport.RoundTrip(request)
}

type upstreams struct {
	codex, bps                     *httptest.Server
	codexHits, bpsHits, attachHits atomic.Int64
	lastBPSBody                    atomic.Value
	lastBPSHeader                  atomic.Value
	lastCodexBody                  atomic.Value
	lastCodexHeaderValue           atomic.Value
}

// newUpstreams 启动假的 codex 和 basispoints 服务器；bps 为 nil 时 basispoints 返回 200 SSE。
func newUpstreams(t *testing.T, bps http.HandlerFunc) *upstreams {
	t.Helper()
	u := &upstreams{}
	u.codex = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.codexHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		u.lastCodexBody.Store(string(body))
		u.lastCodexHeaderValue.Store(r.Header.Clone())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: codex\n\n"))
	}))
	if bps == nil {
		bps = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = w.Write([]byte("data: bps\n\n"))
		}
	}
	u.bps = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			u.attachHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"filename":"x","openai_file_id":"file-UP","size":3}`))
			return
		}
		u.bpsHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		u.lastBPSBody.Store(string(body))
		u.lastBPSHeader.Store(r.Header.Clone())
		bps(w, r)
	}))
	previous := bpsURL
	bpsURL = u.bps.URL + "/basispoints/api/responses"
	t.Cleanup(func() {
		bpsURL = previous
		u.codex.Close()
		u.bps.Close()
	})
	return u
}

func (u *upstreams) forwarder(t *testing.T, mutate func(*config.Config)) *Forwarder {
	t.Helper()
	cfg := config.Default()
	cfg.BPSEnabled = true
	cfg.UseAccountProxy = false
	if mutate != nil {
		mutate(&cfg)
	}
	codex, _ := url.Parse(u.codex.URL)
	return &Forwarder{Pool: NewPool(cfg), Stats: &Stats{}, roundTripper: redirectTransport{codex: codex}}
}

func responsesStream(body string) *fakeStream {
	return newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{
			Method: http.MethodPost,
			Url:    codexURL,
			Host:   "chatgpt.com",
			Headers: map[string]*pluginv1.HeaderValues{
				"Authorization":      {Values: []string{"Bearer tok"}},
				"Chatgpt-Account-Id": {Values: []string{"acct"}},
				"Originator":         {Values: []string{"codex_cli_rs"}},
				"Session_id":         {Values: []string{"sess"}},
				"Content-Type":       {Values: []string{"application/json"}},
			},
			HasBody:       true,
			ContentLength: int64(len(body)),
		}),
		chunkFrame(body[:len(body)/2]), chunkFrame(body[len(body)/2:]), endFrame(),
	)
}

const routableBody = `{"model":"gpt-5.6-sol","stream":true,"instructions":"sys","input":[{"type":"message","role":"user","content":"hi"}],"include":["reasoning.encrypted_content"]}`

func run(t *testing.T, forwarder *Forwarder, stream *fakeStream) (*pluginv1.ForwardResponseStart, string, *pluginv1.ForwardResponseError) {
	t.Helper()
	if err := forwarder.Forward(stream); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	start, body, _, frameErr := collect(t, stream.frames())
	return start, body, frameErr
}

func TestResponsesRoutedToBasispoints(t *testing.T) {
	u := newUpstreams(t, nil)
	forwarder := u.forwarder(t, nil)
	start, body, frameErr := run(t, forwarder, responsesStream(routableBody))
	if frameErr != nil || start.StatusCode != 200 || body != "data: bps\n\n" {
		t.Fatalf("start=%+v body=%q err=%+v", start, body, frameErr)
	}
	if u.bpsHits.Load() != 1 || u.codexHits.Load() != 0 {
		t.Fatalf("hits bps=%d codex=%d", u.bpsHits.Load(), u.codexHits.Load())
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(u.lastBPSBody.Load().(string)), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "gpt-5.6-sol" || sent["model_selection"] != "explicit" || sent["include"] != nil {
		t.Fatalf("body not rewritten: %v", sent)
	}
	header := u.lastBPSHeader.Load().(http.Header)
	if header.Get("X-Basispoints-Auth-Mode") != "chatgpt" || header.Get("Originator") != "" || header.Get("Session_id") != "" {
		t.Fatalf("headers not rewritten: %v", header)
	}
	if header.Get("Authorization") != "Bearer tok" || header.Get("X-Openai-Account-Id") != "acct" {
		t.Fatalf("identity lost: %v", header)
	}
	if forwarder.Stats.RoutedBPS.Load() != 1 || forwarder.Stats.RoutedCodex.Load() != 0 || forwarder.Stats.BPSStatus.Get("200") != 1 {
		t.Fatalf("stats bps=%d codex=%d status=%v", forwarder.Stats.RoutedBPS.Load(), forwarder.Stats.RoutedCodex.Load(), forwarder.Stats.BPSStatus.Snapshot())
	}
}

func TestResponsesSkippedGoToCodexUnchanged(t *testing.T) {
	cases := map[string]struct {
		body   string
		mutate func(*config.Config)
		reason string
	}{
		"disabled":        {routableBody, func(c *config.Config) { c.BPSEnabled = false }, ""},
		"model":           {`{"model":"gpt-5.5","input":"hi"}`, nil, "model_not_allowed"},
		"tools relay off": {`{"model":"gpt-5.6-sol","tools":[{"type":"function","name":"shell"}],"input":"hi"}`, func(c *config.Config) { c.ToolRelay = false }, "has_tools"},
		"tool non-stream": {`{"model":"gpt-5.6-sol","stream":false,"tools":[{"type":"function","name":"shell"}],"input":"hi"}`, nil, "tool_non_stream"},
		"too large":       {`{"model":"gpt-5.6-sol","input":"` + strings.Repeat("x", 2<<20) + `"}`, func(c *config.Config) { c.MaxBodyBytes = 1 << 20 }, "body_too_large"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			u := newUpstreams(t, nil)
			forwarder := u.forwarder(t, tc.mutate)
			start, body, frameErr := run(t, forwarder, responsesStream(tc.body))
			if frameErr != nil || start.StatusCode != 200 || body != "data: codex\n\n" {
				t.Fatalf("start=%+v body=%q err=%+v", start, body, frameErr)
			}
			if u.bpsHits.Load() != 0 || u.codexHits.Load() != 1 {
				t.Fatalf("hits bps=%d codex=%d", u.bpsHits.Load(), u.codexHits.Load())
			}
			if got := u.lastCodexBody.Load().(string); got != tc.body {
				t.Fatalf("codex body changed (len %d vs %d)", len(got), len(tc.body))
			}
			if u.lastCodexHeaderValue.Load().(http.Header).Get("Originator") != "codex_cli_rs" {
				t.Fatal("codex headers must be passed through unchanged")
			}
			if tc.reason != "" && forwarder.Stats.SkipReasons.Get(tc.reason) != 1 {
				t.Fatalf("skip reasons = %v", forwarder.Stats.SkipReasons.Snapshot())
			}
			if forwarder.Stats.RoutedCodex.Load() != 1 {
				t.Fatalf("routed codex = %d", forwarder.Stats.RoutedCodex.Load())
			}
		})
	}
}

func TestNonResponsesPathsPassThroughWithoutBuffering(t *testing.T) {
	u := newUpstreams(t, nil)
	forwarder := u.forwarder(t, nil)
	stream := newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{Method: http.MethodPost, Url: codexURL + "/compact", HasBody: true, ContentLength: 2}),
		chunkFrame("{}"), endFrame(),
	)
	if _, body, frameErr := run(t, forwarder, stream); frameErr != nil || body != "data: codex\n\n" {
		t.Fatalf("body=%q err=%+v", body, frameErr)
	}
	if len(forwarder.Stats.SkipReasons.Snapshot()) != 0 {
		t.Fatal("non-responses paths should not be counted as skips")
	}
}

func TestBasispointsFallbacks(t *testing.T) {
	cases := map[string]struct {
		handler  http.HandlerFunc
		fallback string
	}{
		"model access": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"basispoints_model_access_changed","message":"x"}}`))
		}, fallbackModelAccess},
		"cloudflare": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<!doctype html><title>Access denied</title>`))
		}, fallbackBlocked},
		"422": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"message":"422: Invalid request body."}}`))
		}, fallbackInvalidBody},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			u := newUpstreams(t, tc.handler)
			forwarder := u.forwarder(t, nil)
			start, body, frameErr := run(t, forwarder, responsesStream(routableBody))
			if frameErr != nil || start.StatusCode != 200 || body != "data: codex\n\n" {
				t.Fatalf("expected codex fallback: start=%+v body=%q err=%+v", start, body, frameErr)
			}
			if u.lastCodexBody.Load().(string) != routableBody {
				t.Fatal("fallback must resend the original body")
			}
			if forwarder.Stats.Fallbacks.Get(tc.fallback) != 1 || forwarder.Stats.RoutedCodex.Load() != 1 || forwarder.Stats.RoutedBPS.Load() != 0 {
				t.Fatalf("fallbacks=%v codex=%d bps=%d", forwarder.Stats.Fallbacks.Snapshot(), forwarder.Stats.RoutedCodex.Load(), forwarder.Stats.RoutedBPS.Load())
			}
		})
	}
}

func TestBasispointsErrorsWithoutFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusForbidden} {
		u := newUpstreams(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"other"}}`))
		})
		forwarder := u.forwarder(t, nil)
		start, body, frameErr := run(t, forwarder, responsesStream(routableBody))
		if frameErr != nil || int(start.StatusCode) != status || body != `{"error":{"code":"other"}}` {
			t.Fatalf("status %d: start=%+v body=%q err=%+v", status, start, body, frameErr)
		}
		if u.codexHits.Load() != 0 || forwarder.Stats.RoutedBPS.Load() != 1 {
			t.Fatalf("status %d must be returned as-is, codex hits=%d", status, u.codexHits.Load())
		}
	}
}

func TestFallbackDisabledReturnsBasispointsError(t *testing.T) {
	u := newUpstreams(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`bad`))
	})
	forwarder := u.forwarder(t, func(c *config.Config) { c.FallbackToCodex = false })
	start, body, _ := run(t, forwarder, responsesStream(routableBody))
	if start.StatusCode != http.StatusUnprocessableEntity || body != "bad" || u.codexHits.Load() != 0 {
		t.Fatalf("start=%+v body=%q codex=%d", start, body, u.codexHits.Load())
	}
}

func TestConnectFailureFallsBackToCodex(t *testing.T) {
	u := newUpstreams(t, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + listener.Addr().String() + "/basispoints/api/responses"
	_ = listener.Close()
	bpsURL = dead

	forwarder := u.forwarder(t, nil)
	_, body, frameErr := run(t, forwarder, responsesStream(routableBody))
	if frameErr != nil || body != "data: codex\n\n" || forwarder.Stats.Fallbacks.Get(fallbackConnect) != 1 {
		t.Fatalf("body=%q err=%+v fallbacks=%v", body, frameErr, forwarder.Stats.Fallbacks.Snapshot())
	}
	if forwarder.Stats.BPSStatus.Get("error") != 1 {
		t.Fatalf("bps status = %v", forwarder.Stats.BPSStatus.Snapshot())
	}
}

func TestConnectFailureWithoutFallbackIsRetryable(t *testing.T) {
	u := newUpstreams(t, nil)
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + listener.Addr().String() + "/basispoints/api/responses"
	_ = listener.Close()
	bpsURL = dead

	forwarder := u.forwarder(t, func(c *config.Config) { c.FallbackToCodex = false })
	_, _, frameErr := run(t, forwarder, responsesStream(routableBody))
	if frameErr == nil || frameErr.Code != codeUpstreamFailed || frameErr.RequestSent {
		t.Fatalf("error frame = %+v", frameErr)
	}
}

func TestBasispointsLargeErrorBodyIsRelayedIntact(t *testing.T) {
	large := `{"error":{"code":"other","pad":"` + strings.Repeat("y", errorPeekLimit*2) + `"}}`
	u := newUpstreams(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(large))
	})
	forwarder := u.forwarder(t, nil)
	_, body, _ := run(t, forwarder, responsesStream(routableBody))
	if body != large {
		t.Fatalf("relayed body length %d, want %d", len(body), len(large))
	}
}

func TestFallbackReason(t *testing.T) {
	response := func(status int, contentType string) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}}
	}
	cases := []struct {
		response *http.Response
		body     string
		want     string
	}{
		{response(403, "application/json"), `{"error":{"code":"basispoints_model_access_changed"}}`, fallbackModelAccess},
		{response(403, "text/html"), `x`, fallbackBlocked},
		{response(403, ""), `<HTML><body>blocked</body></HTML>`, fallbackBlocked},
		{response(403, "application/json"), `{"error":{"code":"account_deactivated"}}`, ""},
		{response(422, "application/json"), `{}`, fallbackInvalidBody},
		{response(401, "application/json"), `{}`, ""},
	}
	for i, tc := range cases {
		if got := fallbackReason(tc.response, []byte(tc.body)); got != tc.want {
			t.Errorf("case %d: got %q want %q", i, got, tc.want)
		}
	}
}
