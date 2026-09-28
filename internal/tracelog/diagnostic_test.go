package tracelog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticHeadersPrivacy(t *testing.T) {
	h := make(http.Header)
	for key, value := range map[string]string{"Authorization": "Bearer secret", "Cookie": "secret", "Set-Cookie": "secret", "X-Unknown": "secret", "OpenAI-Organization": "org-private", "Retry-After": "2", "X-Request-Id": "req-123", "X-Ratelimit-Limit-Requests": "1000", "X-Ratelimit-Reset-Tokens": "1m2s", "Content-Type": "text/event-stream; private=secret", "X-Codex-Primary-Used-Percent": "NaN", "Server": "unsafe value"} {
		h.Set(key, value)
	}
	got := SafeHeaders(h)
	if len(got) != 5 || got["content-type"] != "text/event-stream" || got["retry-after"] != "2" {
		t.Fatalf("headers=%v", got)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "org-private") {
		t.Fatal(string(encoded))
	}
	h.Set("Retry-After", "Wed, 21 Oct 2015 07:28:00 GMT")
	if SafeHeaders(h)["retry-after"] != "2015-10-21T07:28:00Z" {
		t.Fatal(SafeHeaders(h))
	}
	if Fingerprint("session", "a") == Fingerprint("workspace", "a") || Fingerprint("workspace", "a") == Fingerprint("workspace", "b") || Fingerprint("session", "") != "" {
		t.Fatal("fingerprint separation")
	}
}

func TestDiagnosticLimitSignals(t *testing.T) {
	for _, body := range []string{`{"detail":"403: Forbidden."}`, `{"error":"403: Forbidden."}`} {
		o := NewObserver(403, "application/json")
		o.Feed([]byte(body))
		got := o.Finish()
		if len(got.Signals) != 1 || got.Signals[0].MessageHash != Fingerprint("error_message", "403: Forbidden.") {
			t.Fatalf("detail/string error: %+v", got)
		}
	}
	headers := safeErrorHeaders(json.RawMessage(`{"retry-after":["9"],"x-request-id":"req-123","authorization":["secret"]}`))
	if len(headers) != 2 || headers["retry-after"] != "9" {
		t.Fatal(headers)
	}
	for _, tc := range []struct {
		status                   int
		contentType, body, class string
		limit, window            int64
	}{
		{429, "application/json", `{"error":{"message":"You've exceeded the 1000 request(s) every 1 minute(s) rate limit."}}`, "rate_limit", 1000, 60},
		{403, "application/json", `{"error":{"message":"Forbidden"}}`, "forbidden_unknown", 0, 0},
		{403, "text/html", `<html>Blocked: private challenge</html>`, "forbidden_html", 0, 0},
		{429, "application/json", `{"error":{"code":"usage_limit_reached","message":"quota exhausted"}}`, "quota_exhausted", 0, 0},
		{403, "application/json", `{"error":{"message":"usage policy violation"}}`, "usage_policy", 0, 0},
	} {
		o := NewObserver(tc.status, tc.contentType)
		o.Feed([]byte(tc.body))
		got := o.Finish()
		if len(got.Signals) != 1 || got.Signals[0].Class != tc.class || got.Signals[0].Status != tc.status {
			t.Fatalf("%s: %+v", tc.class, got)
		}
		if tc.limit > 0 && (got.Signals[0].Limit == nil || *got.Signals[0].Limit != tc.limit || *got.Signals[0].WindowSec != tc.window) {
			t.Fatalf("missing limit: %+v", got)
		}
		encoded, _ := json.Marshal(got)
		if strings.Contains(string(encoded), "private challenge") || strings.Contains(string(encoded), "You've exceeded") {
			t.Fatal("raw error leaked")
		}
	}
}

func TestDiagnosticSSEFragmentationUsageAndDedup(t *testing.T) {
	nl := string([]byte{13, 10})
	errorJSON := `{"code":"rate_limit_exceeded","message":"Rate limit reached for gpt-6-astra in organization org-private on tokens per min (TPM): Limit 500,000,000, Used 499,000,000, Requested 2,000,000."}`
	data := "event: error" + nl + "data: {" + nl + `data: "status":429,"headers":{"retry-after":"5","authorization":"private-token"},"error":` + errorJSON + "}" + nl + nl +
		`data: {"type":"response.failed","response":{"error":` + errorJSON + "}}" + nl + nl +
		`data: {"type":"response.completed","response":{"id":"private-response","model":"gpt-6-astra","usage":{"input_tokens":100,"output_tokens":3,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":2}}}}` + nl + nl
	for _, chunk := range []int{1, 2, 7, len(data)} {
		o := NewObserver(200, "text/event-stream")
		for i := 0; i < len(data); i += chunk {
			end := i + chunk
			if end > len(data) {
				end = len(data)
			}
			o.Feed([]byte(data[i:end]))
		}
		got := o.Finish()
		if got.Events != 3 || len(got.Signals) != 1 || got.Terminal != "response.completed" || got.Usage == nil || got.Usage.Cached == nil || *got.Usage.Cached != 0 || *got.Usage.Input != 100 {
			t.Fatalf("chunk %d: %+v", chunk, got)
		}
		s := got.Signals[0]
		if s.Metric != "tokens" || s.Limit == nil || *s.Limit != 500000000 || *s.Used != 499000000 || *s.Requested != 2000000 || s.Organization != Fingerprint("organization", "org-private") || s.Headers["retry-after"] != "5" || s.Status != 429 {
			t.Fatalf("signal=%+v", s)
		}
		encoded, _ := json.Marshal(got)
		for _, secret := range []string{"org-private", "private-token", "private-response", "Rate limit reached"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("leaked %s", secret)
			}
		}
	}
}

func TestDiagnosticOversizedEventThenLateError(t *testing.T) {
	for _, ending := range []string{string([]byte{10}), string([]byte{13, 10})} {
		o := NewObserver(200, "text/event-stream")
		o.Feed([]byte("data: " + strings.Repeat("x", maxDiagnosticEvent+1)))
		for _, b := range []byte(ending + "data: ignored tail" + ending + ending) {
			o.Feed([]byte{b})
		}
		o.Feed([]byte(`data: {"type":"error","error":{"message":"boom"}}` + ending + ending))
		got := o.Finish()
		if got.Oversized != 1 || got.Events != 1 || len(got.Signals) != 1 || got.Invalid != 0 {
			t.Fatalf("got=%+v", got)
		}
	}
}

func TestDiagnosticUnknownUsageIncompleteAndErrorCap(t *testing.T) {
	nonSSE := NewObserver(200, "application/json")
	nonSSE.Feed([]byte(`{"id":"r","status":"completed","usage":{"input_tokens":42,"input_tokens_details":{"cached_tokens":0}}}`))
	result := nonSSE.Finish()
	if result.Terminal != "response.completed" || result.Usage == nil || *result.Usage.Input != 42 || result.Usage.Cached == nil || *result.Usage.Cached != 0 || result.Invalid != 0 {
		t.Fatalf("non-SSE result=%+v", result)
	}
	o := NewObserver(200, "text/event-stream")
	o.Feed([]byte(sse(`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`)))
	got := o.Finish()
	if got.Usage != nil || got.Terminal != "response.incomplete" || len(got.Signals) != 1 {
		t.Fatalf("%+v", got)
	}
	o = NewObserver(200, "text/event-stream")
	for i := 0; i < 11; i++ {
		o.Feed([]byte(sse(fmt.Sprintf(`{"type":"error","error":{"message":"%d"}}`, i))))
	}
	o.Feed([]byte("data: partial"))
	got = o.Finish()
	if len(got.Signals) != 8 || got.SignalsDropped != 3 || got.Invalid != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestDiagnosticWriterRetentionAndSeparation(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "upstream-20000101-00.jsonl")
	fresh := filepath.Join(dir, "upstream-20000101-01.jsonl")
	for _, path := range []string{old, fresh} {
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	expired := time.Now().Add(-25 * time.Hour)
	_ = os.Chtimes(old, expired, expired)
	w, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	w.LogDiagnostic(UpstreamEvent{Schema: 1, Event: "upstream_start", ForwardID: "f", Time: time.Now().Format(time.RFC3339Nano)})
	w.Log(Record{Entry: Entry{RequestID: "r"}})
	w.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired file remains")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh file removed")
	}
	if w.retention != 24*time.Hour || w.DiagnosticWritten.Load() != 1 || w.Written.Load() != 1 {
		t.Fatal("counts or retention")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	if len(files) != 1 || len(filepath.Base(files[0])) != len("requests-20260929-00.jsonl") {
		t.Fatal(files)
	}
	bodies, _ := filepath.Glob(filepath.Join(dir, "bodies", "*"))
	if len(bodies) != 0 {
		t.Fatal("diagnostics must not write bodies")
	}
	w.LogDiagnostic(UpstreamEvent{})
	if w.DiagnosticDropped.Load() != 1 || w.Dropped.Load() != 1 {
		t.Fatal("closed-writer drops not counted")
	}
	w.Close()
}

func TestDiagnosticWriterDropAndConcurrentClose(t *testing.T) {
	w := &Writer{queue: make(chan Record, 1)}
	w.LogDiagnostic(UpstreamEvent{})
	w.LogDiagnostic(UpstreamEvent{})
	if w.DiagnosticDropped.Load() != 1 {
		t.Fatal("queue drop")
	}
	actual, err := New(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				actual.LogDiagnostic(UpstreamEvent{})
			}
		}()
	}
	actual.Close()
	wg.Wait()
	actual.Close()
	if actual.DiagnosticWritten.Load()+actual.DiagnosticDropped.Load() != 200 {
		t.Fatal("records lost without accounting")
	}
}
