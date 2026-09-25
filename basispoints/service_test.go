package basispoints

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestRegistrationSatisfiesHostMetadata(t *testing.T) {
	raw, errMarshal := json.Marshal(pluginRegistration())
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var decoded struct {
		Metadata struct {
			Name             string
			Version          string
			Author           string
			GitHubRepository string
		}
		Capabilities struct {
			ModelProvider bool `json:"model_provider"`
			ModelRouter   bool `json:"model_router"`
			Executor      bool `json:"executor"`
		}
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if decoded.Metadata.Name == "" || decoded.Metadata.Version == "" || decoded.Metadata.Author == "" || decoded.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata = %#v", decoded.Metadata)
	}
	if !decoded.Capabilities.ModelProvider || !decoded.Capabilities.ModelRouter || !decoded.Capabilities.Executor {
		t.Fatalf("capabilities = %#v", decoded.Capabilities)
	}
}

func TestRouteMatchesSuffixAndSkipsBlockedModels(t *testing.T) {
	svc := New()
	routed := svc.Route("gpt-6-astra(max)")
	if !routed.Handled || routed.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("astra route = %#v", routed)
	}
	if svc.Route("gpt-5.6-sol").Handled != true {
		t.Fatal("expected gpt-5.6-sol to route")
	}
	if svc.Route("gpt-6-sol").Handled || svc.Route("gpt-6-luna").Handled {
		t.Fatal("blocked models must stay on the native path")
	}
}

func TestPrepareBodyClampsEffort(t *testing.T) {
	body, errPrepare := prepareBody([]byte(`{"model":"gpt-6-astra(max)","previous_response_id":"resp_old","reasoning":{"effort":"max"}}`), "gpt-6-astra", "max", "high")
	if errPrepare != nil {
		t.Fatal(errPrepare)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-6-astra" {
		t.Fatalf("model = %s", got)
	}
	if gjson.GetBytes(body, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id leaked: %s", body)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "high" {
		t.Fatalf("effort = %s", got)
	}
	if !gjson.GetBytes(body, "stream").Bool() {
		t.Fatalf("stream = %s", body)
	}

	kept, errKept := prepareBody([]byte(`{"reasoning":{"effort":"medium"}}`), "gpt-5.6-sol", "", "high")
	if errKept != nil {
		t.Fatal(errKept)
	}
	if got := gjson.GetBytes(kept, "reasoning.effort").String(); got != "medium" {
		t.Fatalf("medium effort = %s", got)
	}

	fromSuffix, errSuffix := prepareBody([]byte(`{}`), "gpt-6-astra", "xhigh", "high")
	if errSuffix != nil {
		t.Fatal(errSuffix)
	}
	if got := gjson.GetBytes(fromSuffix, "reasoning.effort").String(); got != "high" {
		t.Fatalf("suffix effort = %s", got)
	}

	none, errNone := prepareBody([]byte(`{"reasoning":{"effort":"none"}}`), "gpt-6-astra", "none", "high")
	if errNone != nil {
		t.Fatal(errNone)
	}
	if gjson.GetBytes(none, "reasoning.effort").Exists() {
		t.Fatalf("none effort remained: %s", none)
	}
}

func TestUpstreamHeaders(t *testing.T) {
	headers := upstreamHeaders("token-1", "acct-1", "chatgpt")
	if headers["authorization"][0] != "Bearer token-1" {
		t.Fatalf("authorization = %v", headers["authorization"])
	}
	if headers["chatgpt-account-id"][0] != "acct-1" || headers["x-openai-account-id"][0] != "acct-1" {
		t.Fatalf("account headers = %#v", headers)
	}
	if headers["x-basispoints-auth-mode"][0] != "chatgpt" {
		t.Fatalf("auth mode = %v", headers["x-basispoints-auth-mode"])
	}
}

func TestAccountIDFromJWT(t *testing.T) {
	token := testJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-jwt"},
	})
	raw := []byte(`{"access_token":"` + token + `"}`)
	gotToken, accountID, ok := credentialFromJSON(raw)
	if !ok || gotToken != token || accountID != "acct-jwt" {
		t.Fatalf("credential = %q %q %v", gotToken, accountID, ok)
	}
	fileToken, fileAccount, okFile := credentialFromJSON([]byte(`{"access_token":"` + token + `","account_id":"acct-file"}`))
	if !okFile || fileToken != token || fileAccount != "acct-file" {
		t.Fatalf("file account = %q %q %v", fileToken, fileAccount, okFile)
	}
}

func TestCompletedEventFromSSE(t *testing.T) {
	raw := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\"}]}}\n\n" +
		"data: [DONE]\n\n")
	event := completedEvent(raw)
	if gjson.GetBytes(event, "type").String() != "response.completed" {
		t.Fatalf("event = %s", event)
	}
	if gjson.GetBytes(event, "response.id").String() != "resp_1" {
		t.Fatalf("response id = %s", event)
	}
}

func TestSSESplitterAcrossChunks(t *testing.T) {
	splitter := sseSplitter{}
	first := splitter.push([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\nda"))
	if len(first) != 1 {
		t.Fatalf("first frames = %d", len(first))
	}
	second := splitter.push([]byte("ta: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n"))
	if len(second) != 1 || gjson.GetBytes(sseData(second[0]), "type").String() != "response.completed" {
		t.Fatalf("second = %s", second)
	}
}

func TestExecuteRotatesAfter429(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	svc := New()
	svc.now = func() time.Time { return now }
	host := &fakeHost{
		auths: []AuthRecord{
			{AuthIndex: "a", Provider: "codex"},
			{AuthIndex: "b", Provider: "codex"},
		},
		files: map[string][]byte{
			"a": []byte(`{"access_token":"token-a","account_id":"acct-a"}`),
			"b": []byte(`{"access_token":"token-b","account_id":"acct-b"}`),
		},
	}
	host.do = func(req UpstreamRequest) (UpstreamResponse, error) {
		if req.Headers["chatgpt-account-id"][0] == "acct-a" {
			return UpstreamResponse{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`), Headers: map[string][]string{"retry-after": {"30"}}}, nil
		}
		return UpstreamResponse{StatusCode: http.StatusOK, Body: []byte(`{"type":"response.completed","response":{"id":"resp_ok","output":[]}}`)}, nil
	}

	resp, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{
		Model:   "gpt-6-astra",
		Payload: []byte(`{"model":"gpt-6-astra","input":"hi"}`),
	})
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	if gjson.GetBytes(resp.Payload, "response.id").String() != "resp_ok" {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if host.callsFor("acct-a") != 1 || host.callsFor("acct-b") != 1 {
		t.Fatalf("calls = %#v", host.calls)
	}

	host.calls = nil
	if _, errAgain := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-5.6-sol", Payload: []byte(`{}`)}); errAgain != nil {
		t.Fatal(errAgain)
	}
	if host.callsFor("acct-a") != 0 || host.callsFor("acct-b") != 1 {
		t.Fatalf("cooled calls = %#v", host.calls)
	}
}

func TestExecuteRetriesUnauthorizedOnce(t *testing.T) {
	svc := New()
	host := &fakeHost{
		auths: []AuthRecord{{AuthIndex: "a", Type: "codex"}},
		files: map[string][]byte{"a": []byte(`{"access_token":"stale","account_id":"acct-a"}`)},
	}
	var gets int
	host.get = func(index string) []byte {
		gets++
		if gets == 1 {
			return host.files[index]
		}
		return []byte(`{"access_token":"fresh","account_id":"acct-a"}`)
	}
	var tokens []string
	host.do = func(req UpstreamRequest) (UpstreamResponse, error) {
		tokens = append(tokens, req.Headers["authorization"][0])
		if req.Headers["authorization"][0] == "Bearer stale" {
			return UpstreamResponse{StatusCode: http.StatusUnauthorized, Body: []byte(`{"error":{"code":"invalid_api_key","message":"expired"}}`)}, nil
		}
		return UpstreamResponse{StatusCode: http.StatusOK, Body: []byte(`{"type":"response.completed","response":{"id":"resp_new"}}`)}, nil
	}
	resp, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(`{}`)})
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	if gjson.GetBytes(resp.Payload, "response.id").String() != "resp_new" {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if len(tokens) != 2 || tokens[0] != "Bearer stale" || tokens[1] != "Bearer fresh" {
		t.Fatalf("tokens = %#v", tokens)
	}
}

func TestExecuteBusyWhenAccountInFlight(t *testing.T) {
	svc := New()
	started := make(chan struct{})
	release := make(chan struct{})
	host := &fakeHost{
		auths: []AuthRecord{{AuthIndex: "a", Provider: "codex"}},
		files: map[string][]byte{"a": []byte(`{"access_token":"token-a","account_id":"acct-a"}`)},
	}
	host.do = func(req UpstreamRequest) (UpstreamResponse, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return UpstreamResponse{StatusCode: http.StatusOK, Body: []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(`{}`)})
		done <- errExecute
	}()
	<-started
	_, errBusy := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(`{}`)})
	var statusErr *StatusError
	if errBusy == nil || !errorAs(errBusy, &statusErr) || statusErr.HTTPStatus != http.StatusTooManyRequests || statusErr.Code != "basispoints_busy" {
		t.Fatalf("busy error = %v", errBusy)
	}
	close(release)
	if errExecute := <-done; errExecute != nil {
		t.Fatal(errExecute)
	}
}

func TestExecuteStreamEmitsSSE(t *testing.T) {
	svc := New()
	host := &fakeHost{
		auths: []AuthRecord{{AuthIndex: "a", Provider: "codex"}},
		files: map[string][]byte{"a": []byte(`{"access_token":"token-a","account_id":"acct-a"}`)},
	}
	host.doStream = func(req UpstreamRequest) (UpstreamStream, error) {
		return UpstreamStream{
			StatusCode: http.StatusOK,
			Stream: &sliceStream{chunks: [][]byte{
				[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"),
				[]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\"}}\n\n"),
			}},
		}, nil
	}
	closed := make(chan struct{})
	host.onClose = func() { close(closed) }
	_, errExecute := svc.ExecuteStream(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(`{}`)}, "stream-1")
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	<-closed
	if len(host.emitted) != 2 {
		t.Fatalf("emitted = %#v", host.emitted)
	}
	if gjson.GetBytes(sseData([]byte(host.emitted[1])), "type").String() != "response.completed" {
		t.Fatalf("last frame = %s", host.emitted[1])
	}
}

func TestExecuteRejectsCredentialWithoutToken(t *testing.T) {
	svc := New()
	host := &fakeHost{
		auths: []AuthRecord{{AuthIndex: "a", Provider: "codex"}},
		files: map[string][]byte{"a": []byte(`{"email":"user@example.com"}`)},
		do: func(UpstreamRequest) (UpstreamResponse, error) {
			t.Fatal("upstream should not be called")
			return UpstreamResponse{}, nil
		},
	}
	_, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(`{}`)})
	var statusErr *StatusError
	if !errorAs(errExecute, &statusErr) || statusErr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("error = %v", errExecute)
	}
}

func TestCountTokensDoesNotCallUpstream(t *testing.T) {
	svc := New()
	host := &fakeHost{}
	resp := svc.CountTokens(pluginapi.ExecutorRequest{Payload: []byte("12345678")})
	if gjson.GetBytes(resp.Payload, "total_tokens").Int() != 2 {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if host.do != nil {
		t.Fatal("count tokens must stay local")
	}
}

func TestParseConfigOverrides(t *testing.T) {
	cfg, errParse := parseConfig([]byte("base_url: https://example.test/basispoints/api/\nauth_mode: chatgpt\nmodels:\n  - gpt-6-astra\nmax_effort: medium\nmax_in_flight_per_account: 2\nmin_interval_ms: 250\ncooldown_ms: 5000\n"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if cfg.BaseURL != "https://example.test/basispoints/api" || cfg.MaxEffort != "medium" || cfg.MaxInFlight != 2 {
		t.Fatalf("cfg = %#v", cfg)
	}
	if _, ok := cfg.canonicalModel("GPT-6-ASTRA(high)"); !ok {
		t.Fatal("expected configured model")
	}
	if _, ok := cfg.canonicalModel("gpt-5.6-sol"); ok {
		t.Fatal("unlisted model must not match")
	}
	if cfg.MinInterval != 250*time.Millisecond || cfg.Cooldown != 5*time.Second {
		t.Fatalf("timing = %s %s", cfg.MinInterval, cfg.Cooldown)
	}
}

type fakeHost struct {
	mu       sync.Mutex
	auths    []AuthRecord
	files    map[string][]byte
	calls    []string
	emitted  []string
	do       func(UpstreamRequest) (UpstreamResponse, error)
	doStream func(UpstreamRequest) (UpstreamStream, error)
	get      func(string) []byte
	onClose  func()
}

func (h *fakeHost) ListAuth(context.Context) ([]AuthRecord, error) {
	return append([]AuthRecord(nil), h.auths...), nil
}

func (h *fakeHost) GetAuth(_ context.Context, index string) ([]byte, error) {
	if h.get != nil {
		return append([]byte(nil), h.get(index)...), nil
	}
	return append([]byte(nil), h.files[index]...), nil
}

func (h *fakeHost) Do(_ context.Context, req UpstreamRequest) (UpstreamResponse, error) {
	h.mu.Lock()
	h.calls = append(h.calls, req.Headers["chatgpt-account-id"][0])
	h.mu.Unlock()
	return h.do(req)
}

func (h *fakeHost) DoStream(_ context.Context, req UpstreamRequest) (UpstreamStream, error) {
	h.mu.Lock()
	h.calls = append(h.calls, req.Headers["chatgpt-account-id"][0])
	h.mu.Unlock()
	return h.doStream(req)
}

func (h *fakeHost) Emit(_ context.Context, _ string, payload []byte) error {
	h.mu.Lock()
	h.emitted = append(h.emitted, string(payload))
	h.mu.Unlock()
	return nil
}

func (h *fakeHost) CloseStream(context.Context, string, string) error {
	if h.onClose != nil {
		h.onClose()
		h.onClose = nil
	}
	return nil
}

func (h *fakeHost) Log(string, string, map[string]string) {}

func (h *fakeHost) callsFor(account string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, call := range h.calls {
		if call == account {
			count++
		}
	}
	return count
}

type sliceStream struct {
	chunks [][]byte
	index  int
}

func (s *sliceStream) Read() ([]byte, error) {
	if s.index >= len(s.chunks) {
		return nil, io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return chunk, nil
}

func (s *sliceStream) Close() error { return nil }

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, errMarshal := json.Marshal(claims)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return "aaa." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func errorAs(err error, target **StatusError) bool {
	status, ok := err.(*StatusError)
	if !ok {
		return false
	}
	*target = status
	return true
}
