package basispoints

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestInlineImageRejectedWhenUploadDisabled(t *testing.T) {
	svc := New()
	host := &fakeHost{
		auths: []AuthRecord{{AuthIndex: "a", Provider: "codex"}},
		files: map[string][]byte{"a": []byte(`{"access_token":"token-a","account_id":"acct-a"}`)},
		do: func(UpstreamRequest) (UpstreamResponse, error) {
			t.Fatal("upstream should not be called")
			return UpstreamResponse{}, nil
		},
	}
	_, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{
		Model:   "gpt-6-astra",
		Payload: []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + onePixelPNG + `"}]}]}`),
	})
	var statusErr *StatusError
	if !errorAs(errExecute, &statusErr) || statusErr.HTTPStatus != 400 || !strings.Contains(statusErr.Message, "does not accept data:image") {
		t.Fatalf("error = %v", errExecute)
	}
}

func TestInlineImageUploadsWhenEnabled(t *testing.T) {
	svc := New()
	if errConfigure := svc.Configure([]byte("image_upload: true\nimage_ttl_seconds: 1800\n")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	images := &recordingImages{}
	svc.imageOverride = images
	var sent []byte
	host := &fakeHost{
		auths: []AuthRecord{{AuthIndex: "a", Provider: "codex"}},
		files: map[string][]byte{"a": []byte(`{"access_token":"token-a","account_id":"acct-a"}`)},
	}
	host.do = func(req UpstreamRequest) (UpstreamResponse, error) {
		sent = append([]byte(nil), req.Body...)
		return UpstreamResponse{StatusCode: 200, Body: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_img\",\"output\":[]}}\n\n")}, nil
	}
	payload := `{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_text","text":"see"},{"type":"input_image","image_url":"data:image/png;base64,` + onePixelPNG + `"}]}]}`
	_, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(payload)})
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	if images.ttl != 30*time.Minute || images.calls != 1 || images.contentType != "image/png" {
		t.Fatalf("upload = %+v", images)
	}
	if strings.Contains(string(sent), "data:image") || !strings.Contains(string(sent), "https://images.example/object.png") {
		t.Fatalf("wire image = %s", sent)
	}
}

func TestImageUploadRequiresStorage(t *testing.T) {
	svc := New()
	if errConfigure := svc.Configure([]byte("image_upload: true\n")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	host := &fakeHost{
		auths: []AuthRecord{{AuthIndex: "a", Provider: "codex"}},
		files: map[string][]byte{"a": []byte(`{"access_token":"token-a","account_id":"acct-a"}`)},
	}
	_, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{
		Model:   "gpt-6-astra",
		Payload: []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + onePixelPNG + `"}]}]}`),
	})
	var statusErr *StatusError
	if !errorAs(errExecute, &statusErr) || statusErr.Code != "basispoints_image_unconfigured" {
		t.Fatalf("error = %v", errExecute)
	}
}

func TestImageProxyClientAcceptsSOCKS(t *testing.T) {
	client, errClient := httpClientForProxy("socks5://127.0.0.1:1")
	if errClient != nil || client == nil || client.Transport == nil {
		t.Fatalf("proxy client = %v %v", client, errClient)
	}
	if _, errDirect := httpClientForProxy(""); errDirect != nil {
		t.Fatal(errDirect)
	}
}

func TestUploadedImageIsDeletedAfterTTL(t *testing.T) {
	blobs := &memoryBlobs{}
	images := newExpiringImages(blobs)
	defer images.Close()
	body := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1}
	url, errUpload := images.Upload(context.Background(), "image/png", body, 30*time.Minute)
	if errUpload != nil || !strings.HasPrefix(url, "https://images.example/") {
		t.Fatalf("upload = %s %v", url, errUpload)
	}
	again, errAgain := images.Upload(context.Background(), "image/png", body, 30*time.Minute)
	if errAgain != nil || again != url || blobs.puts != 1 {
		t.Fatalf("reuse = %s puts=%d err=%v", again, blobs.puts, errAgain)
	}
	images.mu.Lock()
	if len(images.jobs) != 1 {
		images.mu.Unlock()
		t.Fatalf("jobs = %d", len(images.jobs))
	}
	images.jobs[0].at = time.Now().Add(-time.Second)
	images.mu.Unlock()
	images.sweep(time.Now())
	if len(blobs.deleted) != 1 {
		t.Fatalf("deleted = %#v", blobs.deleted)
	}
}

type recordingImages struct {
	mu          sync.Mutex
	calls       int
	ttl         time.Duration
	contentType string
}

func (r *recordingImages) Upload(_ context.Context, contentType string, body []byte, ttl time.Duration) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.ttl = ttl
	r.contentType = contentType
	if len(body) == 0 {
		return "", errImageEmpty
	}
	return "https://images.example/object.png?sig=1", nil
}

var errImageEmpty = statusImage("empty image")

func statusImage(message string) error {
	return &StatusError{Code: "basispoints_image_upload", Message: message, HTTPStatus: 502}
}

type memoryBlobs struct {
	mu      sync.Mutex
	puts    int
	deleted []string
}

func (m *memoryBlobs) Put(context.Context, string, string, []byte) error {
	m.mu.Lock()
	m.puts++
	m.mu.Unlock()
	return nil
}

func (m *memoryBlobs) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://images.example/" + key + "?sig=1", nil
}

func (m *memoryBlobs) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	m.deleted = append(m.deleted, key)
	m.mu.Unlock()
	return nil
}
