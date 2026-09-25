package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jzg-lab/bps_sub_plugin/internal/basispoints"
)

func imageHeader() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer tok")
	h.Set("Chatgpt-Account-Id", "acct")
	return h
}

func TestUploadAttachmentSuccess(t *testing.T) {
	var gotName, gotCT, gotAuth string
	var gotBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		_ = r.ParseMultipartForm(1 << 20)
		file, hdr, err := r.FormFile("file")
		if err == nil {
			gotName = hdr.Filename
			gotBytes, _ = io.ReadAll(file)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"filename":"x","openai_file_id":"file-XYZ","size":3}`))
	}))
	defer srv.Close()
	prev := bpsURL
	bpsURL = srv.URL + "/basispoints/api/responses"
	defer func() { bpsURL = prev }()

	img := basispoints.FoundImage{MediaType: "image/png", Data: []byte("PNG"), SHA256: "abcdef0123456789"}
	fileID, err := uploadAttachment(context.Background(), http.DefaultTransport, imageHeader(), "UA/1", img)
	if err != nil || fileID != "file-XYZ" {
		t.Fatalf("upload = %q, %v", fileID, err)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") || gotAuth != "Bearer tok" {
		t.Fatalf("headers wrong: ct=%q auth=%q", gotCT, gotAuth)
	}
	if gotName != "picture-abcdef012345.png" || string(gotBytes) != "PNG" {
		t.Fatalf("form file wrong: %q %q", gotName, gotBytes)
	}
}

func TestUploadAttachmentHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Payment Required","code":"deactivated_workspace"}}`))
	}))
	defer srv.Close()
	prev := bpsURL
	bpsURL = srv.URL + "/basispoints/api/responses"
	defer func() { bpsURL = prev }()

	_, err := uploadAttachment(context.Background(), http.DefaultTransport, imageHeader(), "UA", basispoints.FoundImage{MediaType: "image/png", Data: []byte("x")})
	if err == nil || !strings.Contains(err.Error(), "deactivated_workspace") {
		t.Fatalf("expected error with code, got %v", err)
	}
	if _, ok := err.(uploadUnavailable); ok {
		t.Fatal("HTTP error must not be uploadUnavailable")
	}
}

func TestUploadAttachmentConnectionFailure(t *testing.T) {
	prev := bpsURL
	bpsURL = "http://127.0.0.1:1/basispoints/api/responses"
	defer func() { bpsURL = prev }()
	_, err := uploadAttachment(context.Background(), http.DefaultTransport, imageHeader(), "UA", basispoints.FoundImage{MediaType: "image/png", Data: []byte("x")})
	if _, ok := err.(uploadUnavailable); !ok {
		t.Fatalf("connection failure should be uploadUnavailable, got %v", err)
	}
}

func TestUploadAttachmentNoFileID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"filename":"x"}`))
	}))
	defer srv.Close()
	prev := bpsURL
	bpsURL = srv.URL + "/basispoints/api/responses"
	defer func() { bpsURL = prev }()
	if _, err := uploadAttachment(context.Background(), http.DefaultTransport, imageHeader(), "UA", basispoints.FoundImage{Data: []byte("x")}); err == nil {
		t.Fatal("missing file id should error")
	}
}

func TestImageCacheLRU(t *testing.T) {
	cache := newImageCache(2)
	cache.put("a", "file-a")
	cache.put("b", "file-b")
	if v, ok := cache.get("a"); !ok || v != "file-a" {
		t.Fatal("a should be present")
	}
	cache.put("c", "file-c") // evicts b (a was just used)
	if _, ok := cache.get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if v, ok := cache.get("c"); !ok || v != "file-c" {
		t.Fatal("c should be present")
	}
}

func TestAttachmentsURLDerivation(t *testing.T) {
	prev := bpsURL
	bpsURL = "https://bps.openai.com/basispoints/api/responses"
	defer func() { bpsURL = prev }()
	if attachmentsURL() != "https://bps.openai.com/basispoints/api/attachments" {
		t.Fatalf("attachments url = %s", attachmentsURL())
	}
}
