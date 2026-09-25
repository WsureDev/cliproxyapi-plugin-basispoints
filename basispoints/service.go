package basispoints

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// StatusError is returned to the host with an HTTP status.
type StatusError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *StatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *StatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// AuthRecord is one host credential visible to the plugin.
type AuthRecord struct {
	AuthIndex      string
	Provider       string
	Type           string
	Disabled       bool
	Unavailable    bool
	NextRetryAfter time.Time
}

// UpstreamRequest is an outbound Basis Points call.
type UpstreamRequest struct {
	Method        string
	URL           string
	Headers       map[string][]string
	Body          []byte
	HeaderProfile []string
}

// UpstreamResponse is a buffered upstream response.
type UpstreamResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

// ByteStream is a pull stream of upstream bytes.
type ByteStream interface {
	Read() ([]byte, error)
	Close() error
}

// UpstreamStream is a streaming upstream response.
type UpstreamStream struct {
	StatusCode int
	Headers    map[string][]string
	Stream     ByteStream
}

// HostClient is the subset of host callbacks the plugin uses.
type HostClient interface {
	ListAuth(context.Context) ([]AuthRecord, error)
	GetAuth(context.Context, string) ([]byte, error)
	Do(context.Context, UpstreamRequest) (UpstreamResponse, error)
	DoStream(context.Context, UpstreamRequest) (UpstreamStream, error)
	Emit(context.Context, string, []byte) error
	CloseStream(context.Context, string, string) error
	Log(string, string, map[string]string)
}

// Service routes selected models through the Basis Points responses API.
type Service struct {
	mu            sync.Mutex
	cfg           Config
	cursor        int
	inflight      map[string]int
	nextAt        map[string]time.Time
	coolUntil     map[string]time.Time
	now           func() time.Time
	replay        *ReplayCache
	aliases       map[string]string
	excluded      map[string]struct{}
	imageOverride imageUploader
	imageStore    *expiringImages
	imageStamp    string
}

func New() *Service {
	return &Service{
		cfg:       defaultConfig(),
		inflight:  map[string]int{},
		nextAt:    map[string]time.Time{},
		coolUntil: map[string]time.Time{},
		now:       time.Now,
		replay:    &ReplayCache{},
		aliases:   map[string]string{},
		excluded:  map[string]struct{}{},
	}
}

func (s *Service) Configure(raw []byte) error {
	cfg, errParse := parseConfig(raw)
	if errParse != nil {
		return errParse
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return nil
}

func (s *Service) snapshot() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Service) currentTime() time.Time {
	s.mu.Lock()
	nowFn := s.now
	s.mu.Unlock()
	if nowFn == nil {
		return time.Now()
	}
	return nowFn()
}

func (s *Service) Registration() registration {
	return pluginRegistration()
}

func (s *Service) StaticModels() pluginapi.ModelResponse {
	return pluginapi.ModelResponse{Provider: providerCodex, Models: s.snapshot().modelInfos()}
}

func (s *Service) Route(model string, body []byte) pluginapi.ModelRouteResponse {
	if _, ok := s.resolveModel(model); !ok {
		return pluginapi.ModelRouteResponse{Handled: false}
	}
	if reason := NativeFallbackReason(body); reason != "" {
		return pluginapi.ModelRouteResponse{Handled: false, Reason: reason}
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "basispoints",
	}
}

func (s *Service) CountTokens(req pluginapi.ExecutorRequest) pluginapi.ExecutorResponse {
	payload := requestPayload(req)
	count := utf8.RuneCount(payload) / 4
	if count < 1 && len(payload) > 0 {
		count = 1
	}
	return pluginapi.ExecutorResponse{
		Payload: []byte(`{"input_tokens":` + strconv.Itoa(count) + `,"total_tokens":` + strconv.Itoa(count) + `}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}
}

func (s *Service) Execute(ctx context.Context, host HostClient, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	body, errCall := s.call(ctx, host, req)
	if errCall != nil {
		return pluginapi.ExecutorResponse{}, errCall
	}
	return pluginapi.ExecutorResponse{
		Payload: completedEvent(body),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// StreamChunk is one SSE frame returned inline when the host did not open a stream bridge.
type StreamChunk struct {
	Payload []byte `json:"Payload,omitempty"`
}

// StreamResult matches the host executor stream RPC envelope.
type StreamResult struct {
	Headers http.Header   `json:"headers,omitempty"`
	Chunks  []StreamChunk `json:"chunks,omitempty"`
}

func (s *Service) ExecuteStream(ctx context.Context, host HostClient, req pluginapi.ExecutorRequest, streamID string) (StreamResult, error) {
	headers := http.Header{"Content-Type": []string{"text/event-stream"}}
	if strings.TrimSpace(streamID) == "" {
		body, errCall := s.call(ctx, host, req)
		if errCall != nil {
			return StreamResult{}, errCall
		}
		frames := streamFrames(body)
		chunks := make([]StreamChunk, 0, len(frames))
		for _, frame := range frames {
			chunks = append(chunks, StreamChunk{Payload: frame})
		}
		return StreamResult{Headers: headers, Chunks: chunks}, nil
	}

	opened, release, errOpen := s.openStream(ctx, host, req)
	if errOpen != nil {
		return StreamResult{}, errOpen
	}
	go s.pump(ctx, host, streamID, opened, release)
	return StreamResult{Headers: headers}, nil
}

type openedStream struct {
	stream ByteStream
	bridge *Bridge
}

func (s *Service) call(ctx context.Context, host HostClient, req pluginapi.ExecutorRequest) ([]byte, error) {
	cfg := s.snapshot()
	canonical, ok := s.resolveModel(requestModel(req))
	if !ok {
		return nil, &StatusError{Code: "model_not_routed", Message: "model is not routed to basispoints", HTTPStatus: http.StatusNotFound}
	}
	skip := map[string]struct{}{}
	var lastErr error
	for {
		release, account, errAcquire := s.acquire(ctx, host, cfg, skip)
		if errAcquire != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errAcquire
		}
		prepared, bridge, errPrepare := s.build(ctx, cfg, req, canonical, account.accountID)
		if errPrepare != nil {
			release.release(s, 0, 0)
			return nil, errPrepare
		}
		status, headers, body, errDo := s.roundTrip(ctx, host, cfg, prepared, account)
		if errDo != nil {
			release.release(s, 0, 0)
			return nil, errDo
		}
		if status == http.StatusUnauthorized {
			status, headers, body, errDo = s.retryUnauthorized(ctx, host, cfg, prepared, account)
			if errDo != nil {
				release.release(s, 0, 0)
				return nil, errDo
			}
		}
		if status == http.StatusTooManyRequests {
			release.release(s, status, retryAfter(headers, s.currentTime()))
			skip[account.key] = struct{}{}
			lastErr = upstreamError(status, body)
			logHost(host, "warn", "basispoints upstream rate limited", map[string]string{
				"auth_index": account.authIndex,
				"model":      prepared.model,
			})
			continue
		}
		release.release(s, status, 0)
		if status < 200 || status >= 300 {
			logHost(host, "warn", "basispoints upstream rejected request", map[string]string{
				"auth_index": account.authIndex,
				"model":      prepared.model,
				"status":     strconv.Itoa(status),
			})
			return nil, upstreamError(status, body)
		}
		translated, errTranslate := translateUpstream(bridge, body)
		if errTranslate != nil {
			return nil, &StatusError{Code: "basispoints_protocol_error", Message: errTranslate.Error(), HTTPStatus: http.StatusBadGateway}
		}
		return translated, nil
	}
}

func (s *Service) openStream(ctx context.Context, host HostClient, req pluginapi.ExecutorRequest) (openedStream, *lease, error) {
	cfg := s.snapshot()
	canonical, ok := s.resolveModel(requestModel(req))
	if !ok {
		return openedStream{}, nil, &StatusError{Code: "model_not_routed", Message: "model is not routed to basispoints", HTTPStatus: http.StatusNotFound}
	}
	skip := map[string]struct{}{}
	var lastErr error
	for {
		release, account, errAcquire := s.acquire(ctx, host, cfg, skip)
		if errAcquire != nil {
			if lastErr != nil {
				return openedStream{}, nil, lastErr
			}
			return openedStream{}, nil, errAcquire
		}
		prepared, bridge, errPrepare := s.build(ctx, cfg, req, canonical, account.accountID)
		if errPrepare != nil {
			release.release(s, 0, 0)
			return openedStream{}, nil, errPrepare
		}
		opened, status, headers, body, errDo := s.roundTripStream(ctx, host, cfg, prepared, account)
		opened.bridge = bridge
		if errDo != nil {
			release.release(s, 0, 0)
			return openedStream{}, nil, errDo
		}
		if status == http.StatusUnauthorized {
			if opened.stream != nil {
				_ = opened.stream.Close()
				opened.stream = nil
			}
			opened, status, headers, body, errDo = s.retryUnauthorizedStream(ctx, host, cfg, prepared, account)
			opened.bridge = bridge
			if errDo != nil {
				release.release(s, 0, 0)
				return openedStream{}, nil, errDo
			}
		}
		if status == http.StatusTooManyRequests {
			if opened.stream != nil {
				_ = opened.stream.Close()
			}
			release.release(s, status, retryAfter(headers, s.currentTime()))
			skip[account.key] = struct{}{}
			lastErr = upstreamError(status, body)
			continue
		}
		if status < 200 || status >= 300 {
			if opened.stream != nil {
				_ = opened.stream.Close()
			}
			release.release(s, status, 0)
			return openedStream{}, nil, upstreamError(status, body)
		}
		if opened.stream == nil {
			release.release(s, 0, 0)
			return openedStream{}, nil, &StatusError{Code: "upstream_error", Message: "basispoints stream is empty", HTTPStatus: http.StatusBadGateway}
		}
		return opened, release, nil
	}
}

type preparedRequest struct {
	model string
	body  []byte
	url   string
}

func requestPayload(req pluginapi.ExecutorRequest) []byte {
	if len(req.Payload) > 0 {
		return req.Payload
	}
	return req.OriginalRequest
}

type credential struct {
	key       string
	authIndex string
	token     string
	accountID string
}

func (s *Service) roundTrip(ctx context.Context, host HostClient, cfg Config, prepared preparedRequest, creds credential) (int, map[string][]string, []byte, error) {
	resp, errDo := host.Do(ctx, upstreamCall(cfg, prepared, creds))
	if errDo != nil {
		return 0, nil, nil, errDo
	}
	return resp.StatusCode, resp.Headers, resp.Body, nil
}

func (s *Service) retryUnauthorized(ctx context.Context, host HostClient, cfg Config, prepared preparedRequest, creds credential) (int, map[string][]string, []byte, error) {
	refreshed, ok := s.reloadCredential(ctx, host, creds.authIndex)
	if !ok {
		return http.StatusUnauthorized, nil, []byte(`{"error":{"message":"codex credential has no access token","type":"auth_error"}}`), nil
	}
	return s.roundTrip(ctx, host, cfg, prepared, refreshed)
}

func (s *Service) roundTripStream(ctx context.Context, host HostClient, cfg Config, prepared preparedRequest, creds credential) (openedStream, int, map[string][]string, []byte, error) {
	resp, errDo := host.DoStream(ctx, upstreamCall(cfg, prepared, creds))
	if errDo != nil {
		return openedStream{}, 0, nil, nil, errDo
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := readStream(resp.Stream)
		return openedStream{}, resp.StatusCode, resp.Headers, body, nil
	}
	return openedStream{stream: resp.Stream}, resp.StatusCode, resp.Headers, nil, nil
}

func (s *Service) retryUnauthorizedStream(ctx context.Context, host HostClient, cfg Config, prepared preparedRequest, creds credential) (openedStream, int, map[string][]string, []byte, error) {
	refreshed, ok := s.reloadCredential(ctx, host, creds.authIndex)
	if !ok {
		return openedStream{}, http.StatusUnauthorized, nil, []byte(`{"error":{"message":"codex credential has no access token","type":"auth_error"}}`), nil
	}
	return s.roundTripStream(ctx, host, cfg, prepared, refreshed)
}

func (s *Service) reloadCredential(ctx context.Context, host HostClient, authIndex string) (credential, bool) {
	raw, errGet := host.GetAuth(ctx, authIndex)
	if errGet != nil {
		return credential{}, false
	}
	token, accountID, ok := credentialFromJSON(raw)
	if !ok {
		return credential{}, false
	}
	return credential{key: authIndex, authIndex: authIndex, token: token, accountID: accountID}, true
}

func upstreamCall(cfg Config, prepared preparedRequest, creds credential) UpstreamRequest {
	return UpstreamRequest{
		Method:        http.MethodPost,
		URL:           prepared.url,
		Headers:       upstreamHeaders(creds.token, creds.accountID, cfg.AuthMode),
		Body:          prepared.body,
		HeaderProfile: append([]string(nil), upstreamHeaderProfile...),
	}
}

func (s *Service) pump(ctx context.Context, host HostClient, streamID string, opened openedStream, release *lease) {
	defer release.release(s, http.StatusOK, 0)
	var reader io.ReadCloser
	if opened.bridge != nil && opened.stream != nil {
		reader = opened.bridge.Stream(&byteStreamReader{stream: opened.stream})
	}
	defer func() {
		if reader != nil {
			_ = reader.Close()
			return
		}
		if opened.stream != nil {
			_ = opened.stream.Close()
		}
	}()
	if reader == nil {
		_ = host.CloseStream(context.Background(), streamID, "basispoints stream is empty")
		return
	}
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	reads := make(chan readChunk, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, errRead := reader.Read(buf)
			chunk := append([]byte(nil), buf[:n]...)
			select {
			case reads <- readChunk{chunk: chunk, err: errRead}:
			case <-done:
				return
			}
			if errRead != nil {
				return
			}
		}
	}()
	splitter := sseSplitter{}
	terminal := false
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	emitFrames := func(frames [][]byte) bool {
		for _, frame := range frames {
			if sseTerminal(frame) {
				terminal = true
			}
			if errEmit := host.Emit(ctx, streamID, frame); errEmit != nil {
				_ = host.CloseStream(context.Background(), streamID, errEmit.Error())
				return false
			}
		}
		return true
	}
	for {
		select {
		case <-done:
			_ = host.CloseStream(context.Background(), streamID, ctx.Err().Error())
			return
		case <-keepalive.C:
			if errEmit := host.Emit(ctx, streamID, []byte(": keepalive\n\n")); errEmit != nil {
				_ = host.CloseStream(context.Background(), streamID, errEmit.Error())
				return
			}
		case res := <-reads:
			if !emitFrames(splitter.push(res.chunk)) {
				return
			}
			if res.err == nil {
				continue
			}
			if !emitFrames(splitter.flush()) {
				return
			}
			if res.err != io.EOF && !errors.Is(res.err, io.ErrUnexpectedEOF) {
				_ = host.CloseStream(context.Background(), streamID, res.err.Error())
				return
			}
			if !terminal {
				_ = host.Emit(ctx, streamID, incompleteStreamEvent)
			}
			_ = host.CloseStream(ctx, streamID, "")
			return
		}
	}
}

type readChunk struct {
	chunk []byte
	err   error
}

var incompleteStreamEvent = []byte("data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"basispoints_stream_incomplete\",\"message\":\"Upstream stream ended before completion\"}}}\n\n")

func sseTerminal(frame []byte) bool {
	switch gjson.GetBytes(sseData(frame), "type").String() {
	case "response.completed", "response.incomplete", "response.failed", "error":
		return true
	default:
		return false
	}
}

func readStream(stream ByteStream) []byte {
	if stream == nil {
		return nil
	}
	defer func() { _ = stream.Close() }()
	var buf bytes.Buffer
	for {
		chunk, errRead := stream.Read()
		if len(chunk) > 0 {
			buf.Write(chunk)
		}
		if errRead != nil {
			break
		}
	}
	return buf.Bytes()
}

func streamFrames(raw []byte) [][]byte {
	splitter := sseSplitter{}
	frames := splitter.push(raw)
	frames = append(frames, splitter.flush()...)
	if len(frames) > 0 {
		return frames
	}
	event := completedEvent(raw)
	if len(event) == 0 {
		return nil
	}
	return [][]byte{formatSSEData(event)}
}

func upstreamError(status int, body []byte) error {
	if status <= 0 {
		status = http.StatusBadGateway
	}
	code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	if code == "basispoints_model_access_changed" {
		return &StatusError{Code: code, Message: "This model is not available on the account's Excel BPS endpoint", HTTPStatus: status}
	}
	if code == "" {
		code = "basispoints_upstream_error"
	}
	return &StatusError{Code: code, Message: "Excel BPS rejected this request; the upstream body was not returned", HTTPStatus: status}
}

func retryAfter(headers map[string][]string, now time.Time) time.Duration {
	value := headerValue(headers, "retry-after")
	if value == "" {
		return 0
	}
	if seconds, errParse := strconv.Atoi(value); errParse == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, errParse := http.ParseTime(value); errParse == nil {
		wait := when.Sub(now)
		if wait > 0 {
			return wait
		}
	}
	return 0
}

func logHost(host HostClient, level, message string, fields map[string]string) {
	if host == nil {
		return
	}
	host.Log(level, message, fields)
}

type lease struct {
	key  string
	once sync.Once
}

func (l *lease) release(s *Service, status int, retryAfterWait time.Duration) {
	if l == nil || s == nil {
		return
	}
	l.once.Do(func() {
		s.finish(l.key, status, retryAfterWait)
	})
}

func (s *Service) finish(key string, status int, retryAfterWait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[key] > 0 {
		s.inflight[key]--
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if s.cfg.MinInterval > 0 {
		s.nextAt[key] = now.Add(s.cfg.MinInterval)
	}
	if status == http.StatusTooManyRequests && s.cfg.Cooldown > 0 {
		s.coolUntil[key] = now.Add(s.cfg.Cooldown)
	}
}

func (s *Service) acquire(ctx context.Context, host HostClient, cfg Config, skip map[string]struct{}) (*lease, credential, error) {
	records, errList := host.ListAuth(ctx)
	if errList != nil {
		return nil, credential{}, errList
	}
	excluded := map[string]struct{}{}
	for {
		index, errPick := s.pick(records, cfg, skip, excluded)
		if errPick != nil {
			if len(excluded) > 0 && errPick == errExhausted {
				return nil, credential{}, &StatusError{
					Code:       "invalid_codex_credential",
					Message:    "codex credential is missing access token or account id",
					HTTPStatus: http.StatusUnauthorized,
				}
			}
			return nil, credential{}, errPick
		}
		raw, errGet := host.GetAuth(ctx, index)
		if errGet != nil {
			excluded[index] = struct{}{}
			s.releaseQuiet(index)
			continue
		}
		token, accountID, ok := credentialFromJSON(raw)
		if !ok {
			excluded[index] = struct{}{}
			s.releaseQuiet(index)
			continue
		}
		return &lease{key: index}, credential{key: index, authIndex: index, token: token, accountID: accountID}, nil
	}
}

func (s *Service) releaseQuiet(key string) {
	(&lease{key: key}).release(s, 0, 0)
}

func (s *Service) pick(records []AuthRecord, cfg Config, skip, excluded map[string]struct{}) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	var eligible []string
	sawProvider := false
	busy := 0
	cooling := 0
	for _, record := range records {
		if !recordMatchesProvider(record, cfg.AuthProvider) || record.Disabled || record.Unavailable {
			continue
		}
		if !record.NextRetryAfter.IsZero() && now.Before(record.NextRetryAfter) {
			continue
		}
		index := strings.TrimSpace(record.AuthIndex)
		if index == "" {
			continue
		}
		sawProvider = true
		if _, omitted := skip[index]; omitted {
			continue
		}
		if _, omitted := excluded[index]; omitted {
			continue
		}
		if until, ok := s.coolUntil[index]; ok && now.Before(until) {
			cooling++
			continue
		}
		if next, ok := s.nextAt[index]; ok && now.Before(next) {
			cooling++
			continue
		}
		if cfg.MaxInFlight > 0 && s.inflight[index] >= cfg.MaxInFlight {
			busy++
			continue
		}
		eligible = append(eligible, index)
	}
	if len(eligible) == 0 {
		if !sawProvider {
			return "", &StatusError{Code: "no_codex_credential", Message: "no Codex credential is available for basispoints", HTTPStatus: http.StatusServiceUnavailable}
		}
		if busy > 0 && cooling == 0 {
			return "", &StatusError{Code: "basispoints_busy", Message: "basispoints account is busy", HTTPStatus: http.StatusTooManyRequests}
		}
		if cooling > 0 || busy > 0 {
			return "", &StatusError{Code: "basispoints_cooling", Message: "basispoints account is cooling down", HTTPStatus: http.StatusTooManyRequests}
		}
		return "", errExhausted
	}
	sort.Strings(eligible)
	index := eligible[s.cursor%len(eligible)]
	s.cursor++
	s.inflight[index]++
	return index, nil
}

var errExhausted = &StatusError{Code: "basispoints_exhausted", Message: "basispoints credentials are exhausted for this request", HTTPStatus: http.StatusTooManyRequests}

func recordMatchesProvider(record AuthRecord, provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = defaultAuthProvider
	}
	return strings.EqualFold(strings.TrimSpace(record.Provider), provider) || strings.EqualFold(strings.TrimSpace(record.Type), provider)
}
