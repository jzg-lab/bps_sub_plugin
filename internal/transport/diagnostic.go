package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/buildinfo"
	"github.com/jzg-lab/bps_sub_plugin/internal/tracelog"
)

var diagnosticActive = struct {
	sync.Mutex
	counts map[string]int64
}{counts: make(map[string]int64)}

// Counts describe this process only, including response-body lifetime. They are
// observations, not semaphores or estimates of an upstream global rate limit.
func startDiagnosticCounts(event tracelog.UpstreamEvent) (map[string]int64, func()) {
	keys := map[string]string{
		"process": "all", "account": strconv.FormatInt(event.AccountID, 10),
		"workspace": event.Identity.Workspace, "user": event.Identity.User,
		"header_session": event.Identity.HeaderSession, "body_session": event.Identity.BodySession,
		"cache_key": event.Identity.CacheKey, "model": event.Model,
		"endpoint": event.Endpoint, "proxy": event.Identity.ProxyEndpoint,
	}
	if event.AccountID <= 0 {
		delete(keys, "account")
	}
	diagnosticActive.Lock()
	snapshot := make(map[string]int64)
	for dimension, key := range keys {
		if key == "" {
			delete(keys, dimension)
			continue
		}
		key = dimension + ":" + key
		keys[dimension] = key
		diagnosticActive.counts[key]++
		snapshot[dimension] = diagnosticActive.counts[key]
	}
	diagnosticActive.Unlock()
	return snapshot, func() {
		diagnosticActive.Lock()
		defer diagnosticActive.Unlock()
		for _, key := range keys {
			diagnosticActive.counts[key]--
			if diagnosticActive.counts[key] == 0 {
				delete(diagnosticActive.counts, key)
			}
		}
	}
}

type diagnosticTransport struct {
	base        http.RoundTripper
	writer      *tracelog.Writer
	info        *traceInfo
	requestID   string
	accountID   int64
	concurrency int32
	attempt     int
}

func endpointKind(u *url.URL) string {
	target, _ := url.Parse(bpsURL)
	if target != nil && u.Host == target.Host {
		if u.Path == target.Path {
			return "bps_responses"
		}
		attachment, _ := url.Parse(attachmentsURL())
		if attachment != nil && u.Path == attachment.Path {
			return "bps_attachments"
		}
	}
	if u.Hostname() == "chatgpt.com" || u.Hostname() == "api.openai.com" {
		switch u.Path {
		case "/backend-api/codex/responses", "/v1/responses", "/responses":
			return "codex_responses"
		default:
			return "codex_other"
		}
	}
	return "other"
}

func (t *diagnosticTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.attempt++
	started := time.Now()
	event := tracelog.UpstreamEvent{
		Schema: tracelog.DiagnosticSchema, Event: "upstream_start",
		Time: started.Format(time.RFC3339Nano), StartedAt: started.Format(time.RFC3339Nano),
		Instance: diagnosticInstance, Host: diagnosticHost, Version: buildinfo.Version,
		ForwardID: t.info.forwardID, RequestID: t.requestID, Attempt: t.attempt,
		AccountID: t.accountID, AccountConcurrency: t.concurrency,
		Identity: t.info.identity, Model: t.info.model, Plan: t.info.plan,
		Endpoint: endpointKind(request.URL), Method: request.Method, RequestBytes: request.ContentLength,
		UserAgentHash: tracelog.Fingerprint("user_agent", request.Header.Get("User-Agent")),
	}
	counts, release := startDiagnosticCounts(event)
	event.InFlight = counts
	t.writer.LogDiagnostic(event)
	state := &diagnosticResponse{event: event, writer: t.writer, started: started, ctx: request.Context(), release: release}
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() {
			state.mu.Lock()
			defer state.mu.Unlock()
			state.event.Wire.HeadersWrittenAt = time.Now().Format(time.RFC3339Nano)
			state.event.Wire.HeadersWrites++
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			state.mu.Lock()
			defer state.mu.Unlock()
			state.event.Wire.RequestWrittenAt = time.Now().Format(time.RFC3339Nano)
			state.event.Wire.RequestWrites++
			state.event.Wire.WriteFailed = state.event.Wire.WriteFailed || info.Err != nil
		},
		GotFirstResponseByte: func() {
			state.mu.Lock()
			defer state.mu.Unlock()
			state.event.Wire.FirstResponseAt = time.Now().Format(time.RFC3339Nano)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			state.mu.Lock()
			defer state.mu.Unlock()
			reused := info.Reused
			state.event.Wire.ConnectionReused = &reused
			state.event.Wire.ConnectionIdleMS = info.IdleTime.Milliseconds()
		},
	}
	// WithClientTrace composes with roundTrip's existing request_sent tracking.
	response, err := t.base.RoundTrip(request.WithContext(httptrace.WithClientTrace(request.Context(), trace)))
	if err != nil {
		state.finish("transport_error", err)
		return response, err
	}
	if response == nil {
		state.finish("nil_response", nil)
		return response, err
	}
	state.mu.Lock()
	state.event.Wire.HeadersReceivedAt = time.Now().Format(time.RFC3339Nano)
	state.event.HTTPStatus = response.StatusCode
	state.event.Protocol = response.Proto
	state.event.Headers = tracelog.SafeHeaders(response.Header)
	state.event.Organization = tracelog.Fingerprint("organization", response.Header.Get("OpenAI-Organization"))
	state.event.Project = tracelog.Fingerprint("project", response.Header.Get("OpenAI-Project"))
	state.observer = tracelog.NewObserver(response.StatusCode, response.Header.Get("Content-Type"))
	state.body = response.Body
	state.mu.Unlock()
	if response.Body == nil {
		state.finish("no_body", nil)
	} else {
		response.Body = state
	}
	return response, nil
}

type diagnosticResponse struct {
	body     io.ReadCloser
	mu       sync.Mutex
	finished bool
	event    tracelog.UpstreamEvent
	observer *tracelog.Observer
	writer   *tracelog.Writer
	started  time.Time
	ctx      context.Context
	release  func()
}

func (r *diagnosticResponse) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	r.mu.Lock()
	if !r.finished {
		r.event.BytesRead += int64(n)
		r.observer.Feed(p[:n])
	}
	r.mu.Unlock()
	if err == io.EOF {
		r.finish("eof", nil)
	} else if err != nil {
		r.finish("read_error", err)
	}
	return n, err
}

func (r *diagnosticResponse) Close() error {
	err := r.body.Close()
	r.finish("close", err)
	return err
}

func (r *diagnosticResponse) finish(reason string, err error) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	now := time.Now()
	r.event.Event = "upstream_finish"
	r.event.Time = now.Format(time.RFC3339Nano)
	r.event.FinishedAt = r.event.Time
	r.event.DurationMS = now.Sub(r.started).Milliseconds()
	r.event.FinishedBy = reason
	r.event.Cancelled = r.ctx.Err() != nil
	r.event.TransportError = diagnosticError(err)
	if r.observer != nil {
		result := r.observer.Finish()
		r.event.Observation = &result
	}
	event := r.event
	r.mu.Unlock()
	r.release()
	r.writer.LogDiagnostic(event)
}

func diagnosticError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "context_cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "network_timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	return "other"
}
