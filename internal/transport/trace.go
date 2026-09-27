package transport

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/basispoints"
	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
	"github.com/jzg-lab/bps_sub_plugin/internal/tracelog"
)

// maxCapturedResponse 是为日志保留的响应字节上限；超出的部分只计数不保存。
const maxCapturedResponse = 16 << 20

// traceInfo 是一次转发过程中收集的排查信息，由各条路径填写，Forward 结束时写日志。
type traceInfo struct {
	route       string // bps / codex
	reason      string
	nativeTools bool
	request     []byte          // 最终发往上游的请求体
	declared    map[string]bool // 客户端声明的工具（走 bps 时才判断未知工具）
	model       string
	session     string
	plan        string
	chatgpt     string
	upstreamErr string
	nudge       bool // 本轮用户消息是"继续 / ？？？"之类
}

// recordingStream 包住宿主的流：把发回宿主的帧原样转发，同时记下状态码、首包时间和响应字节。
type recordingStream struct {
	Stream
	started time.Time
	info    traceInfo

	mu        sync.Mutex
	status    int
	firstByte time.Duration
	bytesOut  int64
	body      []byte
	truncated bool
	frameErr  string
}

func (s *recordingStream) Send(frame *pluginv1.ForwardResponse) error {
	s.mu.Lock()
	switch f := frame.GetFrame().(type) {
	case *pluginv1.ForwardResponse_Start:
		s.status = int(f.Start.GetStatusCode())
	case *pluginv1.ForwardResponse_BodyChunk:
		if s.firstByte == 0 {
			s.firstByte = time.Since(s.started)
		}
		s.bytesOut += int64(len(f.BodyChunk))
		if room := maxCapturedResponse - len(s.body); room > 0 {
			chunk := f.BodyChunk
			if len(chunk) > room {
				chunk = chunk[:room]
				s.truncated = true
			}
			s.body = append(s.body, chunk...)
		} else if len(f.BodyChunk) > 0 {
			s.truncated = true
		}
	case *pluginv1.ForwardResponse_Error:
		s.frameErr = f.Error.GetCode() + ": " + f.Error.GetMessage()
	}
	s.mu.Unlock()
	return s.Stream.Send(frame)
}

// traceOf 取出流上挂的排查信息；没开日志时返回 nil。
func traceOf(stream Stream) *traceInfo {
	if recorder, ok := stream.(*recordingStream); ok {
		return &recorder.info
	}
	return nil
}

// noteRequest 记下请求的身份信息（模型、会话、套餐、账号），不依赖路由结果。
func (t *traceInfo) noteRequest(header http.Header, body []byte) {
	if t == nil {
		return
	}
	var head struct {
		Model          string         `json:"model"`
		PromptCacheKey string         `json:"prompt_cache_key"`
		ClientMetadata map[string]any `json:"client_metadata"`
	}
	_ = json.Unmarshal(body, &head)
	t.model = head.Model
	t.session = head.PromptCacheKey
	if t.session == "" {
		t.session, _ = head.ClientMetadata["session_id"].(string)
	}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) == nil {
		t.nudge = tracelog.IsNudge(tracelog.LastUserText(parsed))
	}
	t.plan = basispoints.PlanType(header)
	if account := strings.TrimSpace(header.Get("Chatgpt-Account-Id")); len(account) > 8 {
		t.chatgpt = account[:8]
	} else {
		t.chatgpt = account
	}
}

// codex 记下这次原样发往 codex，原因是 reason（可空）。
func (t *traceInfo) codex(reason string, body []byte) {
	if t == nil {
		return
	}
	t.route = "codex"
	if reason != "" {
		t.reason = reason
	}
	if body != nil {
		t.request = body
	}
}

// noteUpstreamError 记下 basispoints 的错误（回落前），截断到 500 字节。
func (t *traceInfo) noteUpstreamError(text string) {
	if t == nil {
		return
	}
	if len(text) > 500 {
		text = text[:500]
	}
	t.upstreamErr = strings.TrimSpace(text)
}

// finishTrace 在 Forward 结束时汇总并写一条日志。
func (f *Forwarder) finishTrace(recorder *recordingStream, requestID string, accountID int64, forwardErr error, cancelled bool) {
	writer := f.trace.Load()
	if writer == nil {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	info := recorder.info
	result := tracelog.Analyze(recorder.body, info.declared)
	entry := tracelog.Entry{
		RequestID:      requestID,
		AccountID:      accountID,
		ChatGPTAccount: info.chatgpt,
		Plan:           info.plan,
		Model:          info.model,
		Session:        info.session,
		Route:          info.route,
		Reason:         info.reason,
		NativeTools:    info.nativeTools,
		Status:         recorder.status,
		FirstByteMs:    recorder.firstByte.Milliseconds(),
		DurationMs:     time.Since(recorder.started).Milliseconds(),
		BytesOut:       recorder.bytesOut,
		Outcome:        result.Outcome,
		Tools:          result.Tools,
		UnknownTools:   result.UnknownTools,
		Text:           result.Text,
		UpstreamError:  firstNonEmpty(info.upstreamErr, result.Error),
		Nudge:          info.nudge,
		Cancelled:      cancelled,
		Truncated:      recorder.truncated,
	}
	if entry.Route == "" {
		entry.Route = "codex"
	}
	switch {
	case recorder.frameErr != "":
		entry.Outcome = tracelog.OutcomeError
		entry.Error = recorder.frameErr
	case forwardErr != nil && !cancelled:
		entry.Outcome = tracelog.OutcomeError
		entry.Error = forwardErr.Error()
	}
	abnormal := tracelog.Abnormal(entry.Outcome) || recorder.status >= 400 || isFallback(info.reason)
	if entry.Outcome == tracelog.OutcomeNonSSE && recorder.status < 400 && recorder.status != 0 {
		abnormal = false
	}
	f.Stats.Outcomes.Add(entry.Outcome)
	record := tracelog.Record{Entry: entry, Abnormal: abnormal || info.nudge}
	// 只给走了 basispoints（含回落）的请求存原文：直接发 codex 的和插件无关，Codex 请求体又带着整段历史，
	// 磁盘吃不消。它们仍然有摘要行，可以拿来和 basispoints 的结局对比。
	if entry.Route == "bps" || isFallback(info.reason) {
		record.Request, record.Response = info.request, recorder.body
	}
	writer.Log(record)
}

// isFallback 判断原因码是不是"basispoints 失败后回落 codex"。
func isFallback(reason string) bool {
	switch reason {
	case fallbackModelAccess, fallbackBlocked, fallbackInvalidBody, fallbackConnect, fallbackRewrite, fallbackImageUpload, fallbackUsagePolicy:
		return true
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
