package basispoints

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxInlineImages     = 20
	maxInlineImageBytes = 20 << 20
	maxInlineBatchBytes = 32 << 20
)

type imageUploader interface {
	Upload(ctx context.Context, contentType string, body []byte, ttl time.Duration) (string, error)
}

type keptImage struct {
	url     string
	key     string
	expires time.Time
}

type deleteJob struct {
	key   string
	at    time.Time
	tries int
}

type blobStore interface {
	Put(ctx context.Context, key, contentType string, body []byte) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
}

type expiringImages struct {
	store blobStore
	mu    sync.Mutex
	cache map[string]keptImage
	jobs  []deleteJob
	stop  chan struct{}
	wake  chan struct{}
	once  sync.Once
}

func newExpiringImages(store blobStore) *expiringImages {
	images := &expiringImages{
		store: store,
		cache: map[string]keptImage{},
		stop:  make(chan struct{}),
		wake:  make(chan struct{}, 1),
	}
	go images.loop()
	return images
}

func (e *expiringImages) Close() {
	if e == nil {
		return
	}
	e.once.Do(func() { close(e.stop) })
}

func (e *expiringImages) Upload(ctx context.Context, contentType string, body []byte, ttl time.Duration) (string, error) {
	if e == nil || e.store == nil {
		return "", fmt.Errorf("basispoints image storage is unavailable")
	}
	if ttl <= 0 {
		ttl = defaultImageTTL
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	now := time.Now()
	e.mu.Lock()
	if kept, ok := e.cache[hash]; ok && now.Add(time.Minute).Before(kept.expires) {
		e.mu.Unlock()
		return kept.url, nil
	}
	e.mu.Unlock()

	key := objectKey(contentType)
	if errPut := e.store.Put(ctx, key, contentType, body); errPut != nil {
		return "", errPut
	}
	url, errSign := e.store.PresignGet(ctx, key, ttl)
	if errSign != nil {
		_ = e.store.Delete(context.Background(), key)
		return "", errSign
	}
	expires := now.Add(ttl)
	e.mu.Lock()
	e.cache[hash] = keptImage{url: url, key: key, expires: expires}
	e.jobs = append(e.jobs, deleteJob{key: key, at: expires})
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return url, nil
}

func (e *expiringImages) loop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-e.wake:
			e.sweep(time.Now())
		case now := <-ticker.C:
			e.sweep(now)
		}
	}
}

func (e *expiringImages) sweep(now time.Time) {
	e.mu.Lock()
	due := make([]deleteJob, 0)
	pending := e.jobs[:0]
	for _, job := range e.jobs {
		if !now.Before(job.at) {
			due = append(due, job)
			continue
		}
		pending = append(pending, job)
	}
	e.jobs = pending
	for hash, kept := range e.cache {
		if !now.Before(kept.expires) {
			delete(e.cache, hash)
		}
	}
	e.mu.Unlock()
	for _, job := range due {
		errDelete := e.store.Delete(context.Background(), job.key)
		if errDelete == nil || job.tries >= 4 {
			continue
		}
		job.tries++
		job.at = now.Add(30 * time.Second)
		e.mu.Lock()
		e.jobs = append(e.jobs, job)
		e.mu.Unlock()
	}
}

func objectKey(contentType string) string {
	var buf [16]byte
	_, _ = randRead(buf[:])
	return hex.EncodeToString(buf[:]) + imageExtension(contentType)
}

func imageExtension(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func (s *Service) rewriteImages(ctx context.Context, cfg Config, payload []byte) ([]byte, error) {
	if !bytesContainDataImage(payload) {
		return payload, nil
	}
	if !cfg.ImageUpload {
		return payload, nil
	}
	uploader, errUploader := s.uploader(cfg)
	if errUploader != nil {
		return nil, errUploader
	}
	if uploader == nil {
		return nil, &StatusError{Code: "basispoints_image_unconfigured", Message: "image upload is enabled but S3 storage is not configured", HTTPStatus: http.StatusServiceUnavailable}
	}
	var source object
	if errDecode := decode(payload, &source); errDecode != nil || source == nil {
		return nil, &StatusError{Code: "basispoints_request_invalid", Message: "basispoints request JSON is invalid", HTTPStatus: http.StatusBadRequest}
	}
	input, _ := source["input"].([]any)
	count := 0
	total := 0
	for _, rawItem := range input {
		item, _ := rawItem.(object)
		for _, field := range []string{"content", "output"} {
			if field == "output" && text(item["type"]) != "function_call_output" && text(item["type"]) != "custom_tool_call_output" {
				continue
			}
			parts, _ := item[field].([]any)
			for _, rawPart := range parts {
				part, _ := rawPart.(object)
				if text(part["type"]) != "input_image" {
					continue
				}
				rawURL := text(part["image_url"])
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawURL)), "data:") {
					continue
				}
				if count >= maxInlineImages {
					return nil, imageStatus("basispoints accepts at most 20 inline images per request")
				}
				contentType, body, errDecode := decodeDataImage(rawURL)
				if errDecode != nil {
					return nil, imageStatus(errDecode.Error())
				}
				total += len(body)
				if total > maxInlineBatchBytes {
					return nil, imageStatus("basispoints inline images exceed the 32 MiB request limit")
				}
				url, errUpload := uploader.Upload(ctx, contentType, body, cfg.ImageTTL)
				if errUpload != nil {
					return nil, &imageUploadFailure{
						status: &StatusError{Code: "basispoints_image_upload", Message: "basispoints image upload failed", HTTPStatus: http.StatusBadGateway},
						cause:  errUpload,
					}
				}
				part["image_url"] = url
				count++
			}
		}
	}
	if count == 0 {
		return payload, nil
	}
	encoded, errEncode := json.Marshal(source)
	if errEncode != nil {
		return nil, imageStatus("basispoints image request could not be encoded")
	}
	return encoded, nil
}

func (s *Service) uploader(cfg Config) (imageUploader, error) {
	if s != nil && s.imageOverride != nil {
		return s.imageOverride, nil
	}
	if !cfg.s3Configured() {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp := cfg.imageStamp() + "\x00" + s.proxyURL
	if s.imageStore != nil && s.imageStamp == stamp {
		return s.imageStore, nil
	}
	if s.imageStore != nil {
		s.imageStore.Close()
		s.imageStore = nil
	}
	store, errStore := newS3BlobStore(cfg, s.proxyURL)
	if errStore != nil {
		return nil, errStore
	}
	s.imageStore = newExpiringImages(store)
	s.imageStamp = stamp
	return s.imageStore, nil
}

type imageUploadFailure struct {
	status *StatusError
	cause  error
}

func (e *imageUploadFailure) Error() string {
	if e == nil || e.status == nil {
		return "basispoints image upload failed"
	}
	return e.status.Error()
}

func (e *imageUploadFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *imageUploadFailure) As(target any) bool {
	if e == nil || e.status == nil {
		return false
	}
	out, ok := target.(**StatusError)
	if !ok {
		return false
	}
	*out = e.status
	return true
}

func imageStatus(message string) *StatusError {
	return &StatusError{Code: "basispoints_request_invalid", Message: message, HTTPStatus: http.StatusBadRequest}
}

func bytesContainDataImage(payload []byte) bool {
	return strings.Contains(strings.ToLower(string(payload)), "data:image/")
}

func decodeDataImage(raw string) (string, []byte, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < len("data:") || !strings.EqualFold(raw[:len("data:")], "data:") {
		return "", nil, fmt.Errorf("basispoints inline image requires a base64 image data URL")
	}
	header, payload, ok := strings.Cut(raw[len("data:"):], ",")
	if !ok || !strings.HasSuffix(strings.ToLower(header), ";base64") {
		return "", nil, fmt.Errorf("basispoints inline image requires a base64 image data URL")
	}
	contentType := strings.ToLower(strings.TrimSpace(header[:len(header)-len(";base64")]))
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return "", nil, fmt.Errorf("basispoints inline images must be PNG, JPEG, GIF or WebP")
	}
	if payload == "" || len(payload) > base64.StdEncoding.EncodedLen(maxInlineImageBytes) {
		return "", nil, fmt.Errorf("basispoints inline image exceeds the 20 MiB limit")
	}
	body, errDecode := base64.StdEncoding.DecodeString(payload)
	if errDecode != nil || len(body) == 0 || len(body) > maxInlineImageBytes {
		return "", nil, fmt.Errorf("basispoints inline image contains invalid base64 data")
	}
	if !imageMagic(contentType, body) {
		return "", nil, fmt.Errorf("basispoints inline image media type does not match its contents")
	}
	return contentType, body, nil
}

func imageMagic(contentType string, body []byte) bool {
	switch contentType {
	case "image/png":
		return len(body) >= 8 && string(body[:8]) == "\x89PNG\r\n\x1a\n"
	case "image/jpeg":
		return len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[2] == 0xff
	case "image/gif":
		return len(body) >= 6 && (string(body[:6]) == "GIF87a" || string(body[:6]) == "GIF89a")
	case "image/webp":
		return len(body) >= 12 && string(body[:4]) == "RIFF" && string(body[8:12]) == "WEBP"
	default:
		return false
	}
}
