package basispoints

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func imageBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDecodeDataURL(t *testing.T) {
	mt, data, ok := DecodeDataURL(dataURL("image/png", []byte("hello")))
	if !ok || mt != "image/png" || string(data) != "hello" {
		t.Fatalf("decode failed: %q %q %v", mt, data, ok)
	}
	if _, _, ok := DecodeDataURL("data:image/png,notbase64flagged"); ok {
		t.Fatal("non-base64 data URL should fail")
	}
	if _, _, ok := DecodeDataURL("https://example.com/x.png"); ok {
		t.Fatal("http url should fail")
	}
	// default media type when omitted
	if mt, _, ok := DecodeDataURL("data:;base64," + base64.StdEncoding.EncodeToString([]byte("x"))); !ok || mt != "application/octet-stream" {
		t.Fatalf("default media type wrong: %q %v", mt, ok)
	}
}

func TestFindImagesDedup(t *testing.T) {
	png := dataURL("image/png", []byte("PNGDATA"))
	body := imageBody(t, `{"input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"look"},
			{"type":"input_image","image_url":"`+png+`"},
			{"type":"input_image","image_url":"`+png+`"}
		]},
		{"type":"message","role":"user","content":[{"type":"input_image","image_url":"`+dataURL("image/jpeg", []byte("JPG"))+`"}]}
	]}`)
	found := FindImages(body)
	if len(found) != 2 {
		t.Fatalf("expected 2 unique images, got %d", len(found))
	}
	if found[0].MediaType != "image/png" || string(found[0].Data) != "PNGDATA" {
		t.Fatalf("first image wrong: %+v", found[0])
	}
	if found[1].MediaType != "image/jpeg" {
		t.Fatalf("second image wrong: %+v", found[1])
	}
	// remote image is not "found" (it's handled as other-attachment upstream)
	remote := imageBody(t, `{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`)
	if len(FindImages(remote)) != 0 {
		t.Fatal("remote image must not be collected for upload")
	}
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestReplaceImages(t *testing.T) {
	png := dataURL("image/png", []byte("PNGDATA"))
	bad := dataURL("image/gif", []byte("GIFDATA"))
	body := imageBody(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"look"},
			{"type":"input_image","image_url":"`+png+`","detail":"high"},
			{"type":"input_image","image_url":"`+bad+`"}
		]}
	]}`)
	results := map[string]ImageResult{
		digestOf([]byte("PNGDATA")): {FileID: "file-123"},
		digestOf([]byte("GIFDATA")): {OmitReason: "upload failed"},
	}
	out := ReplaceImages(body, results)
	content := out["input"].([]any)[0].(map[string]any)["content"].([]any)
	img := content[1].(map[string]any)
	if img["file_id"] != "file-123" || img["detail"] != "high" || img["image_url"] != nil {
		t.Fatalf("uploaded image not rewritten: %v", img)
	}
	omitted := content[2].(map[string]any)
	if omitted["type"] != "input_text" || !strings.Contains(omitted["text"].(string), "upload failed") {
		t.Fatalf("failed image not omitted: %v", omitted)
	}
	// text part untouched
	if content[0].(map[string]any)["text"] != "look" {
		t.Fatal("text part changed")
	}
	// original body not mutated
	origImg := body["input"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)
	if origImg["image_url"] == nil {
		t.Fatal("ReplaceImages must not mutate the original body")
	}
}

func TestReplaceImagesDefaultDetail(t *testing.T) {
	png := dataURL("image/png", []byte("X"))
	body := imageBody(t, `{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"`+png+`"}]}]}`)
	out := ReplaceImages(body, map[string]ImageResult{digestOf([]byte("X")): {FileID: "file-x"}})
	img := out["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if img["detail"] != "auto" {
		t.Fatalf("default detail should be auto: %v", img)
	}
}

func TestImageFileName(t *testing.T) {
	name := ImageFileName("image/jpeg", "abcdef0123456789")
	if name != "picture-abcdef012345.jpg" {
		t.Fatalf("file name = %s", name)
	}
	if ImageFileName("application/octet-stream", "xy") != "picture-xy.png" {
		t.Fatal("unknown media type should default to png")
	}
}

func TestDecideRoutesImages(t *testing.T) {
	png := dataURL("image/png", []byte("PNG"))
	decision := Decide(enabledConfig(), identityHeader(), []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"`+png+`"}]}]}`))
	if !decision.Route || !decision.HasImages {
		t.Fatalf("image request should route with HasImages: %+v", decision)
	}
}
