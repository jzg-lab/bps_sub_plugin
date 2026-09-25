package basispoints

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// 支持的图片类型 → 文件扩展名。
var imageExtensions = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// FoundImage 是 input 里的一张待上传图片。
type FoundImage struct {
	MediaType string
	Data      []byte
	SHA256    string
}

// ImageResult 是一张图片的处理结果：上传成功给 FileID，失败给 OmitReason。
type ImageResult struct {
	FileID     string
	OmitReason string
}

// FindImages 遍历 input，找出所有内联图片（input_image + data: URL）。按 sha256 去重。
func FindImages(body map[string]any) []FoundImage {
	items, ok := body["input"].([]any)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var found []FoundImage
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			if kind, _ := typed["type"].(string); kind == "input_image" {
				if url, ok := typed["image_url"].(string); ok && strings.HasPrefix(url, "data:") {
					if mediaType, data, ok := DecodeDataURL(url); ok {
						sum := sha256.Sum256(data)
						digest := hex.EncodeToString(sum[:])
						if !seen[digest] {
							seen[digest] = true
							found = append(found, FoundImage{MediaType: mediaType, Data: data, SHA256: digest})
						}
					}
				}
				return
			}
			for _, item := range typed {
				walk(item)
			}
		}
	}
	for _, item := range items {
		walk(item)
	}
	return found
}

// ReplaceImages 把 input 里的内联图片换成 file_id 引用或降级文本。
// results 按图片 sha256 索引；没有对应结果的图片保持原样（不应发生）。
func ReplaceImages(body map[string]any, results map[string]ImageResult) map[string]any {
	items, ok := body["input"].([]any)
	if !ok {
		return body
	}
	var replace func(any) any
	replace = func(value any) any {
		switch typed := value.(type) {
		case []any:
			out := make([]any, len(typed))
			for i, item := range typed {
				out[i] = replace(item)
			}
			return out
		case map[string]any:
			if kind, _ := typed["type"].(string); kind == "input_image" {
				if url, ok := typed["image_url"].(string); ok && strings.HasPrefix(url, "data:") {
					if _, data, ok := DecodeDataURL(url); ok {
						sum := sha256.Sum256(data)
						digest := hex.EncodeToString(sum[:])
						result, known := results[digest]
						if !known {
							return typed
						}
						if result.OmitReason != "" {
							return map[string]any{"type": "input_text", "text": "[image content omitted: " + result.OmitReason + "]"}
						}
						picture := map[string]any{}
						for key, v := range typed {
							if key != "image_url" {
								picture[key] = v
							}
						}
						if _, has := picture["detail"]; !has {
							picture["detail"] = "auto"
						}
						picture["file_id"] = result.FileID
						return picture
					}
				}
				return typed
			}
			out := make(map[string]any, len(typed))
			for key, v := range typed {
				out[key] = replace(v)
			}
			return out
		default:
			return value
		}
	}
	newItems := make([]any, len(items))
	for i, item := range items {
		newItems[i] = replace(item)
	}
	out := make(map[string]any, len(body))
	for key, v := range body {
		out[key] = v
	}
	out["input"] = newItems
	return out
}

// DecodeDataURL 解析 data:<media_type>;base64,<payload>，返回 media_type 和字节。
func DecodeDataURL(url string) (string, []byte, bool) {
	header, payload, ok := strings.Cut(url, ",")
	if !ok || !strings.Contains(header, ";base64") {
		return "", nil, false
	}
	mediaType := strings.TrimPrefix(header, "data:")
	if idx := strings.Index(mediaType, ";"); idx >= 0 {
		mediaType = mediaType[:idx]
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// 有的 data URL 用了 URL-safe 或省略 padding，再试一次宽松解码。
		data, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "="))
		if err != nil {
			return "", nil, false
		}
	}
	return mediaType, data, true
}

// ImageFileName 按 media_type 和哈希生成上传文件名。
func ImageFileName(mediaType, digest string) string {
	ext := imageExtensions[mediaType]
	if ext == "" {
		ext = "png"
	}
	short := digest
	if len(short) > 12 {
		short = short[:12]
	}
	return "picture-" + short + "." + ext
}
