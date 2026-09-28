package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/tracelog"
)

func readDiagnostics(t *testing.T, dir string) []tracelog.UpstreamEvent {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "upstream-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var events []tracelog.UpstreamEvent
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), string([]byte{10})) {
			var event tracelog.UpstreamEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
	}
	return events
}

func TestDiagnosticIdentityDoesNotConflateSessionOrLeakSecrets(t *testing.T) {
	payload := `{"sub":"private-subject","exp":2000000000,"https://api.openai.com/auth":{"chatgpt_account_id":"workspace-b","chatgpt_user_id":"private-user"}}`
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+token)
	h.Set("Chatgpt-Account-Id", "workspace-a")
	h.Set("Session_id", "header-session")
	h.Set("Conversation_id", "private-thread")
	identity := requestIdentity(h, "http://proxy-user:proxy-password@proxy.private:8123/path?token=secret", true)
	info := traceInfo{identity: identity}
	info.noteRequest(h, []byte(`{"model":"gpt-6-astra","prompt_cache_key":"cache-only","client_metadata":{"session_id":"body-session"}}`))
	if !identity.JWTParsed || identity.WorkspaceMatch == nil || *identity.WorkspaceMatch || identity.JWTWorkspace == identity.Workspace || identity.TokenExpiresAt == "" || identity.ExitIPKnown || identity.ProxyEndpoint == "" || identity.ProxyIdentity == "" {
		t.Fatalf("identity=%+v", identity)
	}
	if info.identity.CacheKey == info.identity.HeaderSession || info.identity.HeaderSession == info.identity.BodySession || info.identity.BodySession == "" {
		t.Fatal("session/cache conflated")
	}
	encoded, _ := json.Marshal(info.identity)
	for _, secret := range []string{token, "workspace-a", "workspace-b", "proxy.private", "proxy-user", "proxy-password", "private-user", "private-thread", "cache-only", "header-session", "body-session"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	h.Set("Authorization", "Bearer opaque-secret")
	if got := requestIdentity(h, "http://proxy.private", false); got.JWTParsed || got.ProxyMode != "direct" || got.ProxyEndpoint != "" {
		t.Fatalf("got=%+v", got)
	}
}

func TestDiagnosticFallbackPreservesBothAttemptsAndWireTimes(t *testing.T) {
	u := newUpstreams(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-upstream")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Set-Cookie", "private-cookie")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(usagePolicyBody))
	})
	f, read, dir := tracedForwarder(t, u, nil)
	run(t, f, responsesStream(routableBody))
	summaries := read()
	events := readDiagnostics(t, dir)
	if len(events) != 4 || len(summaries) != 1 || u.bpsHits.Load() != 1 || u.codexHits.Load() != 1 {
		t.Fatalf("events=%+v", events)
	}
	if events[0].Event != "upstream_start" || events[1].Event != "upstream_finish" || events[2].Attempt != 2 || events[0].ForwardID != summaries[0].ForwardID || events[1].Endpoint != "bps_responses" || events[3].Endpoint != "codex_responses" || events[1].HTTPStatus != 403 || events[3].HTTPStatus != 200 {
		t.Fatalf("events=%+v", events)
	}
	e := events[1]
	if e.Headers["retry-after"] != "7" || e.Headers["set-cookie"] != "" || e.Observation.Signals[0].Class != "usage_policy" || e.Wire.HeadersWrites != 1 || e.Wire.RequestWrites != 1 {
		t.Fatalf("event=%+v", e)
	}
	for _, field := range []string{e.StartedAt, e.Wire.HeadersWrittenAt, e.Wire.RequestWrittenAt, e.Wire.FirstResponseAt, e.Wire.HeadersReceivedAt, e.FinishedAt, summaries[0].StartedAt, summaries[0].FinishedAt} {
		if _, err := time.Parse(time.RFC3339Nano, field); err != nil {
			t.Fatalf("invalid time %q", field)
		}
	}
}

func TestDiagnosticAttachmentAndCacheReuse(t *testing.T) {
	u := newUpstreams(t, nil)
	f, read, dir := tracedForwarder(t, u, nil)
	f.SetImageCache(10)
	body := `{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,UE5H"}]}]}`
	run(t, f, responsesStream(body))
	run(t, f, responsesStream(body))
	read()
	events := readDiagnostics(t, dir)
	if len(events) != 6 || u.attachHits.Load() != 1 || u.bpsHits.Load() != 2 || events[0].Endpoint != "bps_attachments" || events[2].Endpoint != "bps_responses" || events[4].Attempt != 1 {
		t.Fatalf("events=%+v", events)
	}
}

func TestDiagnosticDoesNotFallbackOrRetryGeneric403And429(t *testing.T) {
	for _, status := range []int{403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			u := newUpstreams(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"Forbidden"}}`))
			})
			f, read, dir := tracedForwarder(t, u, nil)
			response, body, errFrame := run(t, f, responsesStream(routableBody))
			read()
			events := readDiagnostics(t, dir)
			if response.GetStatusCode() != int32(status) || errFrame != nil || !strings.Contains(body, "Forbidden") || u.codexHits.Load() != 0 || u.bpsHits.Load() != 1 || len(events) != 2 || events[1].HTTPStatus != status {
				t.Fatalf("unexpected behavior: response=%+v error=%+v events=%+v", response, errFrame, events)
			}
		})
	}
}

type diagnosticRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn diagnosticRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type diagnosticBody struct {
	io.Reader
	reads, closes     int
	readErr, closeErr error
}

func (b *diagnosticBody) Read(p []byte) (int, error) {
	b.reads++
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.Reader.Read(p)
}
func (b *diagnosticBody) Close() error { b.closes++; return b.closeErr }

func TestDiagnosticTransparentBodyTraceCompositionAndCancellation(t *testing.T) {
	dir := t.TempDir()
	w, err := tracelog.New(tracelog.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &diagnosticBody{Reader: strings.NewReader("unread-private-body")}
	transport := &diagnosticTransport{writer: w, info: &traceInfo{forwardID: "f"}, base: diagnosticRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(r.Context()).WroteHeaders()
		return &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
	})}
	request, _ := http.NewRequestWithContext(ctx, "GET", codexURL, nil)
	response, sent, err := roundTrip(ctx, transport, request)
	if err != nil || !sent || body.reads != 0 {
		t.Fatal("observation changed send semantics or eagerly read")
	}
	cancel()
	_ = response.Body.Close()
	w.Close()
	events := readDiagnostics(t, dir)
	if body.reads != 0 || body.closes != 1 || len(events) != 2 || !events[1].Cancelled || events[1].FinishedBy != "close" {
		t.Fatalf("events=%+v", events)
	}
}

func TestDiagnosticSSEBytesUsageAndHTTP200Failure(t *testing.T) {
	raw := sse("error", map[string]any{"type": "error", "error": map[string]any{"code": "rate_limit_exceeded", "message": "limit"}}) + sse("response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"usage": map[string]any{"input_tokens": 100, "input_tokens_details": map[string]any{"cached_tokens": 80}}}})
	dir := t.TempDir()
	w, _ := tracelog.New(tracelog.Options{Dir: dir})
	body := &diagnosticBody{Reader: strings.NewReader(raw)}
	transport := &diagnosticTransport{writer: w, info: &traceInfo{forwardID: "f"}, base: diagnosticRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
	})}
	request, _ := http.NewRequest("GET", codexURL, nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	if err != nil || string(got) != raw {
		t.Fatal("response bytes changed")
	}
	_ = response.Body.Close()
	w.Close()
	events := readDiagnostics(t, dir)
	e := events[1]
	if len(events) != 2 || e.HTTPStatus != 200 || e.Observation.Terminal != "response.failed" || len(e.Observation.Signals) == 0 || *e.Observation.Usage.Cached != 80 || e.BytesRead != int64(len(raw)) || e.FinishedBy != "eof" {
		t.Fatalf("event=%+v", e)
	}
}

func TestDiagnosticErrorsAndInflightLifecycle(t *testing.T) {
	dir := t.TempDir()
	w, _ := tracelog.New(tracelog.Options{Dir: dir})
	failure := errors.New("private error with proxy password")
	body := &diagnosticBody{Reader: strings.NewReader(""), readErr: io.ErrUnexpectedEOF, closeErr: failure}
	transport := &diagnosticTransport{writer: w, info: &traceInfo{forwardID: "f", identity: tracelog.Identity{Workspace: "workspace-hash"}}, accountID: 99, base: diagnosticRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})}
	request, _ := http.NewRequest("GET", codexURL, nil)
	first, _ := transport.RoundTrip(request)
	second, _ := transport.RoundTrip(request)
	if _, err := first.Body.Read(make([]byte, 10)); err != io.ErrUnexpectedEOF {
		t.Fatal("read error changed")
	}
	if err := first.Body.Close(); err != failure {
		t.Fatal("close error changed")
	}
	_ = second.Body.Close()
	transport.base = diagnosticRoundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, failure })
	if _, err := transport.RoundTrip(request); err != failure {
		t.Fatal("transport error changed")
	}
	w.Close()
	events := readDiagnostics(t, dir)
	if len(events) != 6 || events[0].InFlight["account"] != 1 || events[1].InFlight["account"] != 2 || events[2].TransportError != "unexpected_eof" || events[4].InFlight["account"] != 1 || events[5].TransportError != "other" {
		t.Fatalf("events=%+v", events)
	}
	diagnosticActive.Lock()
	active := len(diagnosticActive.counts)
	diagnosticActive.Unlock()
	if active != 0 {
		t.Fatalf("leaked counts: %d", active)
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "proxy password") {
		t.Fatal("raw transport error leaked")
	}
}
