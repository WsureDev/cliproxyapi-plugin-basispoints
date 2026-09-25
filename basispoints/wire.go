package basispoints

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (s *Service) RememberHost(host pluginapi.HostConfigSummary) {
	aliases := map[string]string{}
	for provider, entries := range host.OAuthModelAlias {
		if !strings.EqualFold(provider, providerCodex) {
			continue
		}
		for _, entry := range entries {
			alias := strings.ToLower(strings.TrimSpace(entry.Alias))
			name := strings.TrimSpace(entry.Name)
			if alias == "" || name == "" {
				continue
			}
			aliases[alias] = name
		}
	}
	excluded := map[string]struct{}{}
	for provider, names := range host.ExcludedModels {
		if !strings.EqualFold(provider, providerCodex) {
			continue
		}
		for _, name := range names {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" {
				excluded[name] = struct{}{}
			}
		}
	}
	s.mu.Lock()
	s.aliases = aliases
	s.excluded = excluded
	s.mu.Unlock()
}

func (s *Service) resolveModel(model string) (string, bool) {
	s.mu.Lock()
	cfg := s.cfg
	aliases := s.aliases
	excluded := s.excluded
	s.mu.Unlock()
	return matchConfiguredModel(cfg, model, aliases, excluded)
}

func matchConfiguredModel(cfg Config, model string, aliases map[string]string, excluded map[string]struct{}) (string, bool) {
	base, _, _ := splitModelSuffix(model)
	base = strings.TrimSpace(base)
	if base == "" {
		return "", false
	}
	if _, skip := excluded[strings.ToLower(base)]; skip {
		return "", false
	}
	candidates := []string{base}
	if mapped := strings.TrimSpace(aliases[strings.ToLower(base)]); mapped != "" {
		candidates = append(candidates, mapped)
	}
	if slash := strings.LastIndex(base, "/"); slash >= 0 && slash < len(base)-1 {
		candidates = append(candidates, base[slash+1:])
	}
	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if _, skip := excluded[key]; skip {
			continue
		}
		if canonical, ok := cfg.Models[key]; ok {
			return canonical, true
		}
	}
	return "", false
}

func requestModel(req pluginapi.ExecutorRequest) string {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(gjson.GetBytes(requestPayload(req), "model").String())
	}
	return model
}

func (s *Service) build(ctx context.Context, cfg Config, req pluginapi.ExecutorRequest, canonical, accountID string) (preparedRequest, *Bridge, error) {
	original := requestPayload(req)
	payload := append([]byte(nil), original...)
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	updated, errSet := sjson.SetBytes(payload, "model", canonical)
	if errSet != nil {
		return preparedRequest{}, nil, errSet
	}
	if strings.TrimSpace(gjson.GetBytes(updated, "reasoning.effort").String()) == "" && strings.TrimSpace(gjson.GetBytes(updated, "reasoning_effort").String()) == "" {
		_, suffix, ok := splitModelSuffix(req.Model)
		if !ok {
			_, suffix, ok = splitModelSuffix(gjson.GetBytes(original, "model").String())
		}
		if ok {
			updated, errSet = sjson.SetBytes(updated, "reasoning.effort", suffix)
			if errSet != nil {
				return preparedRequest{}, nil, errSet
			}
		}
	}
	rewritten, errImages := s.rewriteImages(ctx, cfg, updated)
	if errImages != nil {
		return preparedRequest{}, nil, errImages
	}
	updated = rewritten
	body, bridge, errPrepare := Prepare(updated, requestScope(accountID, req.Headers, updated), s.replay)
	if errPrepare != nil {
		return preparedRequest{}, nil, &StatusError{
			Code:       "basispoints_request_invalid",
			Message:    errPrepare.Error(),
			HTTPStatus: http.StatusBadRequest,
		}
	}
	effort := gjson.GetBytes(body, "reasoning_effort").String()
	clamped := clampEffort(effort, cfg.MaxEffort)
	if clamped != "" && clamped != effort {
		body, errSet = sjson.SetBytes(body, "reasoning_effort", clamped)
		if errSet != nil {
			return preparedRequest{}, nil, errSet
		}
		bridge.Effort = clamped
	}
	return preparedRequest{model: canonical, body: body, url: cfg.BaseURL + "/responses"}, bridge, nil
}

func requestScope(accountID string, headers map[string][]string, body []byte) string {
	thread := headerValue(headers, "thread-id")
	if thread == "" {
		thread = headerValue(headers, "session-id")
	}
	if thread == "" {
		thread = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	return fingerprint([]any{accountID, thread, headerValue(headers, "authorization")})
}

func translateUpstream(bridge *Bridge, raw []byte) ([]byte, error) {
	if bridge == nil {
		return raw, nil
	}
	reader := bridge.Stream(io.NopCloser(bytes.NewReader(raw)))
	defer func() { _ = reader.Close() }()
	translated, errRead := io.ReadAll(reader)
	if errRead != nil && errRead != io.EOF {
		return translated, errRead
	}
	return translated, nil
}

type byteStreamReader struct {
	stream ByteStream
	buf    []byte
	err    error
}

func (r *byteStreamReader) Read(p []byte) (int, error) {
	if r == nil {
		return 0, io.EOF
	}
	for len(r.buf) == 0 && r.err == nil {
		if r.stream == nil {
			r.err = io.EOF
			break
		}
		chunk, errRead := r.stream.Read()
		r.buf = chunk
		if errRead != nil {
			r.err = errRead
		}
	}
	if len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *byteStreamReader) Close() error {
	if r == nil || r.stream == nil {
		return nil
	}
	return r.stream.Close()
}
