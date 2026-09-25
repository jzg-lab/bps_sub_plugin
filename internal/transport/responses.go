package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/basispoints"
	"github.com/jzg-lab/bps_sub_plugin/internal/config"
)

// 回落原因码，出现在状态统计里。
const (
	fallbackModelAccess = "model_access_changed"
	fallbackBlocked     = "blocked_html"
	fallbackInvalidBody = "invalid_body_422"
	fallbackConnect     = "connect_error"
	fallbackRewrite     = "rewrite_error"
	fallbackImageUpload = "image_upload_failed"
)

// errorPeekLimit 是为判断是否回落而读取的错误响应体上限。
const errorPeekLimit = 64 * 1024

// bpsURL 可在测试中替换为本地服务器。
var bpsURL = basispoints.URL

func isResponsesCandidate(request *http.Request) bool {
	return basispoints.IsResponsesRequest(request.Method, request.URL)
}

// forwardResponses 处理一个可能改走 basispoints 的 Responses 请求。
// 请求体需要完整读入内存：既要解析做路由判定，回落 codex 时也要重发原始请求体。
func (f *Forwarder) forwardResponses(ctx context.Context, stream Stream, transport http.RoundTripper, cfg config.Config, request *http.Request, body io.Reader, contentLength int64) error {
	buffered, err := io.ReadAll(io.LimitReader(body, cfg.MaxBodyBytes+1))
	if err != nil {
		return f.fail(stream, codeRequestBody, err.Error(), false)
	}
	if int64(len(buffered)) > cfg.MaxBodyBytes {
		// 太大不改写：已读的前缀接上剩余部分，原样流式发往 codex。
		f.Stats.SkipReasons.Add(basispoints.ReasonBodyTooLarge)
		f.Stats.RoutedCodex.Add(1)
		setStreamingBody(request, io.MultiReader(bytes.NewReader(buffered), body), contentLength)
		return f.sendAndRelay(ctx, stream, transport, request, false)
	}

	decision := basispoints.Decide(cfg, request.Header, buffered)
	if !decision.Route {
		f.Stats.SkipReasons.Add(decision.Reason)
		return f.sendCodex(ctx, stream, transport, request, buffered, false)
	}

	if decision.HasImages {
		if allFailed := f.uploadImages(ctx, transport, request.Header, cfg, decision.Body); allFailed && cfg.FallbackToCodex {
			// 全部图片都传不上去：带图片走原生 codex 更稳。
			f.Stats.Fallbacks.Add(fallbackImageUpload)
			return f.sendCodex(ctx, stream, transport, request, buffered, false)
		}
	}

	bpsBody, err := basispoints.BuildBody(decision, f.replayer)
	if err != nil {
		f.Stats.Fallbacks.Add(fallbackRewrite)
		return f.sendCodex(ctx, stream, transport, request, buffered, false)
	}
	wantStream := true
	if value, ok := decision.Body["stream"].(bool); ok {
		wantStream = value
	}
	bpsRequest, err := newBPSRequest(ctx, request.Header, bpsBody, cfg.BPSUserAgent, wantStream)
	if err != nil {
		f.Stats.Fallbacks.Add(fallbackRewrite)
		return f.sendCodex(ctx, stream, transport, request, buffered, false)
	}

	started := time.Now()
	response, sent, err := roundTrip(ctx, transport, bpsRequest)
	if err != nil {
		f.Stats.BPSStatus.Add("error")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cfg.FallbackToCodex {
			f.Stats.Fallbacks.Add(fallbackConnect)
			// 请求头已写出时 basispoints 可能已开始处理，回落后的最终失败也不能让宿主重放。
			return f.sendCodex(ctx, stream, transport, request, buffered, sent)
		}
		f.Stats.RoutedBPS.Add(1)
		return f.fail(stream, codeUpstreamFailed, err.Error(), sent)
	}
	f.Stats.BPSStatus.Add(statusKey(response.StatusCode))

	if cfg.FallbackToCodex && (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnprocessableEntity) {
		peeked, _ := io.ReadAll(io.LimitReader(response.Body, errorPeekLimit))
		if reason := fallbackReason(response, peeked); reason != "" {
			_ = response.Body.Close()
			f.Stats.Fallbacks.Add(reason)
			return f.sendCodex(ctx, stream, transport, request, buffered, false)
		}
		// 不回落：把读出的部分接回去，原样回传。
		response.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(peeked), response.Body), Closer: response.Body}
	}
	f.Stats.RoutedBPS.Add(1)
	if decision.HasToolContext {
		return f.relayToolStream(stream, response, started, decision)
	}
	return f.relay(stream, response, started)
}

// sendCodex 用缓冲的原始请求体把请求原样发往 codex。
func (f *Forwarder) sendCodex(ctx context.Context, stream Stream, transport http.RoundTripper, request *http.Request, body []byte, priorSent bool) error {
	f.Stats.RoutedCodex.Add(1)
	codex := request.Clone(ctx)
	codex.Body = io.NopCloser(bytes.NewReader(body))
	codex.ContentLength = int64(len(body))
	codex.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return f.sendAndRelay(ctx, stream, transport, codex, priorSent)
}

func newBPSRequest(ctx context.Context, original http.Header, body []byte, userAgent string, stream bool) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, bpsURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header = basispoints.BuildHeaders(original, userAgent, stream)
	if parsed, err := url.Parse(bpsURL); err == nil {
		request.Host = parsed.Host
	}
	return request, nil
}

// fallbackReason 判断 basispoints 的 403/422 是否应回落 codex，返回原因码；不回落返回空串。
// 401（令牌）、429（限流）、5xx（上游故障）都不回落，交给宿主处理。
func fallbackReason(response *http.Response, body []byte) string {
	switch response.StatusCode {
	case http.StatusUnprocessableEntity:
		return fallbackInvalidBody
	case http.StatusForbidden:
		mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if mediaType == "text/html" {
			return fallbackBlocked
		}
		var payload struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Error.Code == "basispoints_model_access_changed" {
			return fallbackModelAccess
		}
		if bytes.Contains(bytes.ToLower(body), []byte("<html")) {
			return fallbackBlocked
		}
	}
	return ""
}

func statusKey(code int) string {
	return strconv.Itoa(code)
}

type readCloser struct {
	io.Reader
	io.Closer
}
