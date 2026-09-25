package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jzg-lab/bps_sub_plugin/internal/config"
	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

func imageReqBody(t *testing.T) string {
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	return `{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"` + png + `"}]}]}`
}

func imageStream(body string) *fakeStream {
	return newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{
			Method: http.MethodPost, Url: codexURL, Host: "chatgpt.com",
			Headers: map[string]*pluginv1.HeaderValues{
				"Authorization": {Values: []string{"Bearer tok"}}, "Chatgpt-Account-Id": {Values: []string{"acct"}},
			},
			HasBody: true, ContentLength: int64(len(body)),
		}),
		chunkFrame(body), endFrame(),
	)
}

func TestImageRequestUploadsAndReferences(t *testing.T) {
	u := newUpstreams(t, nil)
	forwarder := u.forwarder(t, nil)
	forwarder.SetImageCache(16)
	body := imageReqBody(t)
	stream := imageStream(body)
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	if u.attachHits.Load() != 1 || u.bpsHits.Load() != 1 {
		t.Fatalf("hits attach=%d bps=%d", u.attachHits.Load(), u.bpsHits.Load())
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(u.lastBPSBody.Load().(string)), &sent)
	raw, _ := json.Marshal(sent)
	if strings.Contains(string(raw), "data:image") {
		t.Fatal("inline image data URL must not reach basispoints")
	}
	if !strings.Contains(string(raw), "file-UP") {
		t.Fatalf("uploaded file_id not referenced: %s", raw)
	}
	if forwarder.Stats.ImagesUploaded.Load() != 1 {
		t.Fatalf("images_uploaded = %d", forwarder.Stats.ImagesUploaded.Load())
	}
}

func TestImageCacheAvoidsReupload(t *testing.T) {
	u := newUpstreams(t, nil)
	forwarder := u.forwarder(t, nil)
	forwarder.SetImageCache(16)
	body := imageReqBody(t)
	if err := forwarder.Forward(imageStream(body)); err != nil {
		t.Fatal(err)
	}
	if err := forwarder.Forward(imageStream(body)); err != nil {
		t.Fatal(err)
	}
	if u.attachHits.Load() != 1 {
		t.Fatalf("second request should reuse cache, attach hits = %d", u.attachHits.Load())
	}
	if forwarder.Stats.ImagesReused.Load() != 1 {
		t.Fatalf("images_reused = %d", forwarder.Stats.ImagesReused.Load())
	}
}

func TestImageUploadFailureFallsBackToCodex(t *testing.T) {
	u := newUpstreams(t, nil)
	// make the attachments endpoint fail
	u.bps.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			u.attachHits.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			return
		}
		u.bpsHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: bps\n\n"))
	})
	forwarder := u.forwarder(t, nil)
	forwarder.SetImageCache(16)
	stream := imageStream(imageReqBody(t))
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	_, body, _, _ := collect(t, stream.frames())
	if u.codexHits.Load() != 1 || body != "data: codex\n\n" {
		t.Fatalf("all-images-failed should fall back to codex; codex hits=%d body=%q", u.codexHits.Load(), body)
	}
	if forwarder.Stats.Fallbacks.Get(fallbackImageUpload) != 1 {
		t.Fatalf("image upload fallback not counted: %v", forwarder.Stats.Fallbacks.Snapshot())
	}
}

func TestImageSupportOffGoesToCodex(t *testing.T) {
	u := newUpstreams(t, nil)
	forwarder := u.forwarder(t, func(c *config.Config) { c.ImageSupport = false })
	stream := imageStream(imageReqBody(t))
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	if u.attachHits.Load() != 0 || u.codexHits.Load() != 1 {
		t.Fatalf("image_support off: attach=%d codex=%d", u.attachHits.Load(), u.codexHits.Load())
	}
	_, body, _, _ := collect(t, stream.frames())
	if body != "data: codex\n\n" {
		t.Fatalf("expected codex passthrough, got %q", body)
	}
	if forwarder.Stats.SkipReasons.Get("has_attachment") != 1 {
		t.Fatalf("skip reason = %v", forwarder.Stats.SkipReasons.Snapshot())
	}
}
