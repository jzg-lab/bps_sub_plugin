package transport

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/jzg-lab/bps_sub_plugin/internal/basispoints"
	"github.com/jzg-lab/bps_sub_plugin/internal/config"
)

// attachmentsURL 由 bpsURL 派生（把 /responses 换成 /attachments），测试里随 bpsURL 变。
func attachmentsURL() string {
	base := bpsURL
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		return base[:idx] + "/attachments"
	}
	return base
}

// uploadUnavailable 表示上传根本没发出去（连接失败）：本次请求其它图片也不用再试。
type uploadUnavailable struct{ err error }

func (e uploadUnavailable) Error() string { return e.err.Error() }

// imageCache 按 (账号ID, 图片sha256) → file_id 缓存上传结果，避免重复上传。进程内 LRU。
type imageCache struct {
	mu    sync.Mutex
	cache map[string]*list.Element
	lru   *list.List
	limit int
}

func newImageCache(limit int) *imageCache {
	return &imageCache{cache: map[string]*list.Element{}, lru: list.New(), limit: limit}
}

type imageEntry struct {
	key    string
	fileID string
}

func (c *imageCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.cache[key]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*imageEntry).fileID, true
	}
	return "", false
}

func (c *imageCache) put(key, fileID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.cache[key]; ok {
		el.Value.(*imageEntry).fileID = fileID
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(&imageEntry{key: key, fileID: fileID})
	c.cache[key] = el
	for c.lru.Len() > c.limit {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.lru.Remove(oldest)
		delete(c.cache, oldest.Value.(*imageEntry).key)
	}
}

// uploadAttachment 把一张图片以 Excel 插件"上传文件"的方式发到 basispoints 附件端点，
// 返回 openai_file_id。连接失败返回 uploadUnavailable。
func uploadAttachment(ctx context.Context, transport http.RoundTripper, header http.Header, userAgent string, img basispoints.FoundImage) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", basispoints.ImageFileName(img.MediaType, img.SHA256))
	if err != nil {
		return "", err
	}
	if _, err := part.Write(img.Data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, attachmentsURL(), &buf)
	if err != nil {
		return "", err
	}
	// 复用 basispoints 身份头，但换成 multipart 的 content-type 和 json accept。
	request.Header = basispoints.BuildHeaders(header, userAgent, false)
	request.Header.Del("Content-Type")
	request.Header.Set("Content-Type", mw.FormDataContentType())
	request.Header.Set("Accept", "application/json")
	if parsed, err := url.Parse(attachmentsURL()); err == nil {
		request.Host = parsed.Host
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		return "", uploadUnavailable{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if response.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d: %s", response.StatusCode, attachmentErrorMessage(body))
	}
	var parsed struct {
		FileID string `json:"openai_file_id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || strings.TrimSpace(parsed.FileID) == "" {
		return "", errors.New("附件端点未返回 openai_file_id")
	}
	return strings.TrimSpace(parsed.FileID), nil
}

func attachmentErrorMessage(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Error.Message != "" {
			if parsed.Error.Code != "" {
				return parsed.Error.Message + " (" + parsed.Error.Code + ")"
			}
			return parsed.Error.Message
		}
		if parsed.Detail != "" {
			return parsed.Detail
		}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200]
	}
	if text == "" {
		return "no details"
	}
	return text
}

// uploadImages 把 decision.Body 里的内联图片上传到 basispoints，并就地把图片换成 file_id 引用
// 或降级文本。返回 allFailed：存在图片但全部失败（调用方据此决定是否回落 codex）。
func (f *Forwarder) uploadImages(ctx context.Context, transport http.RoundTripper, header http.Header, cfg config.Config, body map[string]any) (allFailed bool) {
	images := basispoints.FindImages(body)
	if len(images) == 0 {
		return false
	}
	accountID := strings.TrimSpace(header.Get("Chatgpt-Account-Id"))
	results := make(map[string]basispoints.ImageResult, len(images))
	var uploadDown bool // 连接失败后，本次请求剩余图片直接降级
	ok, failed := 0, 0
	for _, img := range images {
		if int64(len(img.Data)) > cfg.MaxImageBytes {
			results[img.SHA256] = basispoints.ImageResult{OmitReason: "image too large"}
			failed++
			continue
		}
		cacheKey := accountID + ":" + img.SHA256
		if f.imageCache != nil {
			if fileID, hit := f.imageCache.get(cacheKey); hit {
				results[img.SHA256] = basispoints.ImageResult{FileID: fileID}
				f.Stats.ImagesReused.Add(1)
				ok++
				continue
			}
		}
		if uploadDown {
			results[img.SHA256] = basispoints.ImageResult{OmitReason: "upload unavailable"}
			failed++
			continue
		}
		fileID, err := uploadAttachment(ctx, transport, header, cfg.BPSUserAgent, img)
		if err != nil {
			f.Stats.ImageUploadErrors.Add(1)
			f.Stats.ImagesOmitted.Add(1)
			results[img.SHA256] = basispoints.ImageResult{OmitReason: "could not be uploaded"}
			failed++
			if _, unavailable := err.(uploadUnavailable); unavailable {
				uploadDown = true
			}
			continue
		}
		if f.imageCache != nil {
			f.imageCache.put(cacheKey, fileID)
		}
		results[img.SHA256] = basispoints.ImageResult{FileID: fileID}
		f.Stats.ImagesUploaded.Add(1)
		ok++
	}
	body["input"] = basispoints.ReplaceImages(body, results)["input"]
	return ok == 0 && failed > 0
}
