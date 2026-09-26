package basispoints

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/net/proxy"
)

func randRead(buf []byte) (int, error) {
	return rand.Read(buf)
}

type s3BlobStore struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
	prefix  string
}

func newS3BlobStore(cfg Config, proxyURL string) (*s3BlobStore, error) {
	if !cfg.s3Configured() {
		return nil, fmt.Errorf("basispoints image storage is not configured")
	}
	httpClient, errClient := httpClientForProxy(proxyURL)
	if errClient != nil {
		return nil, errClient
	}
	client := s3.New(s3.Options{
		Region:       cfg.S3Region,
		BaseEndpoint: aws.String(cfg.S3Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		UsePathStyle: true,
		HTTPClient:   httpClient,
	})
	return &s3BlobStore{
		client:  client,
		presign: s3.NewPresignClient(client),
		bucket:  cfg.S3Bucket,
		prefix:  cfg.S3Prefix,
	}, nil
}

func (s *s3BlobStore) objectKey(name string) string {
	if s.prefix == "" {
		return name
	}
	return s.prefix + "/" + name
}

func (s *s3BlobStore) Put(ctx context.Context, key, contentType string, body []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_, errPut := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.objectKey(key)),
		Body:          bytes.NewReader(body),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if errPut != nil {
		return &imageUploadError{cause: errPut}
	}
	return nil
}

func (s *s3BlobStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ttl <= 0 {
		ttl = defaultImageTTL
	}
	signed, errSign := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	}, func(options *s3.PresignOptions) {
		options.Expires = ttl
	})
	if errSign != nil || signed == nil || signed.URL == "" {
		if errSign == nil {
			errSign = fmt.Errorf("empty presigned url")
		}
		return "", &imageUploadError{cause: errSign}
	}
	return signed.URL, nil
}

func (s *s3BlobStore) Delete(ctx context.Context, key string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_, errDelete := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if errDelete != nil {
		return errDelete
	}
	return nil
}

func httpClientForProxy(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" || strings.EqualFold(proxyURL, "direct") {
		return http.DefaultClient, nil
	}
	parsed, errParse := url.Parse(proxyURL)
	if errParse != nil || parsed.Host == "" {
		return nil, fmt.Errorf("basispoints image proxy url is invalid")
	}
	dialer, errDialer := proxy.FromURL(parsed, proxy.Direct)
	if errDialer != nil {
		return nil, fmt.Errorf("basispoints image proxy url is invalid")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if ctx == nil {
				ctx = context.Background()
			}
			if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
				return contextDialer.DialContext(ctx, network, addr)
			}
			return dialer.Dial(network, addr)
		},
	}
	return &http.Client{Transport: transport}, nil
}

type imageUploadError struct {
	cause error
}

func (e *imageUploadError) Error() string { return "basispoints image upload failed" }

func (e *imageUploadError) Unwrap() error { return e.cause }

func redactS3Error(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if idx := strings.Index(msg, "?"); idx >= 0 {
		msg = msg[:idx]
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
