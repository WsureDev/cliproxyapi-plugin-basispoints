package basispoints

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
	routed := svc.Route("gpt-6-astra(max)", nil)
	if !routed.Handled || routed.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("astra route = %#v", routed)
	}
	if svc.Route("gpt-5.6-sol", nil).Handled != true {
		t.Fatal("expected gpt-5.6-sol to route")
	}
	if svc.Route("gpt-6-sol", nil).Handled || svc.Route("gpt-6-luna", nil).Handled {
		t.Fatal("blocked models must stay on the native path")
	}
}

func TestRouteAliasExclusionAndNativeFallback(t *testing.T) {
	svc := New()
	svc.RememberHost(pluginapi.HostConfigSummary{
		OAuthModelAlias: map[string][]pluginapi.ModelAlias{
			"codex": {{Name: "gpt-6-astra", Alias: "astra"}},
		},
		ExcludedModels: map[string][]string{"codex": {"gpt-5.6-sol"}},
	})
	if !svc.Route("astra", nil).Handled {
		t.Fatal("alias should route to the configured upstream model")
	}
	if svc.Route("gpt-5.6-sol", nil).Handled {
		t.Fatal("excluded model must stay on the native path")
	}
	if svc.Route("codex/gpt-6-astra", nil).Handled != true {
		t.Fatal("provider prefix should resolve to the allowlist")
	}
	body := []byte(`{"tools":[{"type":"image_generation"}]}`)
	if route := svc.Route("gpt-6-astra", body); route.Handled || route.Reason != "image_generation" {
		t.Fatalf("image generation route = %#v", route)
	}
}

func TestEmptyModelListRoutesNothing(t *testing.T) {
	svc := New()
	if errConfigure := svc.Configure([]byte("models: []\n")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	if svc.Route("gpt-6-astra", nil).Handled {
		t.Fatal("empty allowlist must not take over")
	}
	if len(svc.StaticModels().Models) != 0 {
		t.Fatal("empty allowlist must not publish models")
	}
}

func TestUpstreamErrorDoesNotEchoBody(t *testing.T) {
	errUpstream := upstreamError(http.StatusForbidden, []byte(`{"error":{"code":"basispoints_model_access_changed","message":"Bearer secret-token"}}`))
	var statusErr *StatusError
	if !errorAs(errUpstream, &statusErr) || statusErr.Code != "basispoints_model_access_changed" || strings.Contains(statusErr.Message, "secret-token") {
		t.Fatalf("model access error = %v", errUpstream)
	}
	errOther := upstreamError(422, []byte(`{"error":{"message":"request echoed secret"}}`))
	if !errorAs(errOther, &statusErr) || strings.Contains(statusErr.Message, "secret") || statusErr.Code != "basispoints_upstream_error" {
		t.Fatalf("generic error = %v", errOther)
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
	if headers["x-openai-internal-basispoints-client-product"][0] != "basispoints-excel-plugin" || headers["x-openai-internal-basispoints-client-agent-profile"][0] != "excel" {
		t.Fatalf("excel profile headers = %#v", headers)
	}
	for _, name := range upstreamHeaderProfile {
		if _, ok := headers[name]; !ok {
			t.Fatalf("header profile omits %s", name)
		}
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
		return UpstreamResponse{StatusCode: http.StatusOK, Body: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"output\":[]}}\n\n")}, nil
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
	if _, errAgain := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-5.6-sol", Payload: []byte(`{"input":"hi"}`)}); errAgain != nil {
		t.Fatal(errAgain)
	}
	if host.callsFor("acct-a") != 1 || host.callsFor("acct-b") != 1 {
		t.Fatalf("retry calls = %#v", host.calls)
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
		return UpstreamResponse{StatusCode: http.StatusOK, Body: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_new\",\"output\":[]}}\n\n")}, nil
	}
	resp, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(`{"input":"hi"}`)})
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
	if errConfigure := svc.Configure([]byte("max_in_flight_per_account: 1\n")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
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
		return UpstreamResponse{StatusCode: http.StatusOK, Body: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n")}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(`{"input":"hi"}`)})
		done <- errExecute
	}()
	<-started
	_, errBusy := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(`{"input":"hi"}`)})
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
	_, errExecute := svc.ExecuteStream(context.Background(), host, pluginapi.ExecutorRequest{Model: "gpt-6-astra", Payload: []byte(`{"input":"hi"}`)}, "stream-1")
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

func TestExecuteRewritesCodexBody(t *testing.T) {
	svc := New()
	if errConfigure := svc.Configure([]byte("max_effort: high\n")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	var sent []byte
	host := &fakeHost{
		auths: []AuthRecord{{AuthIndex: "a", Provider: "codex"}},
		files: map[string][]byte{"a": []byte(`{"access_token":"token-a","account_id":"acct-a"}`)},
	}
	host.do = func(req UpstreamRequest) (UpstreamResponse, error) {
		sent = append([]byte(nil), req.Body...)
		return UpstreamResponse{StatusCode: http.StatusOK, Body: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_wire\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n")}, nil
	}
	payload := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","description":"run js"}]}]},{"type":"message","role":"user","id":"msg_1","content":[{"type":"input_text","text":"hi"}],"internal_chat_message_metadata_passthrough":{"turn_id":"t"}}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"max","context":"all_turns"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"low"},"client_metadata":{"session_id":"s"},"prompt_cache_key":"thread-1","store":false,"stream":true}`)
	resp, errExecute := svc.Execute(context.Background(), host, pluginapi.ExecutorRequest{
		Model:   "gpt-5.6-sol",
		Headers: http.Header{"Thread-Id": []string{"thread-1"}},
		Payload: payload,
	})
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	if gjson.GetBytes(resp.Payload, "response.id").String() != "resp_wire" {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if gjson.GetBytes(sent, "model").String() != "gpt-5.6-sol" || gjson.GetBytes(sent, "model_selection").String() != "explicit" {
		t.Fatalf("model wire = %s", sent)
	}
	if gjson.GetBytes(sent, "reasoning_effort").String() != "high" {
		t.Fatalf("effort = %s", gjson.GetBytes(sent, "reasoning_effort").String())
	}
	for _, field := range []string{"reasoning", "include", "text", "client_metadata", "tool_choice", "parallel_tool_calls", "tools"} {
		if gjson.GetBytes(sent, field).Exists() {
			t.Fatalf("%s leaked: %s", field, sent)
		}
	}
	if strings.Contains(string(sent), "additional_tools") || strings.Contains(string(sent), "internal_chat_message_metadata_passthrough") {
		t.Fatalf("codex item leaked: %s", sent)
	}
	if !strings.HasPrefix(gjson.GetBytes(sent, "prompt_cache_key").String(), "bps-") {
		t.Fatalf("cache key = %s", gjson.GetBytes(sent, "prompt_cache_key").String())
	}
	if !strings.Contains(gjson.GetBytes(sent, "input.0.content.0.text").String(), "functions.exec") {
		t.Fatalf("catalog missing: %s", sent)
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
