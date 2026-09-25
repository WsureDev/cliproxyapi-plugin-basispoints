package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/wsure/cliproxyapi-plugin-basispoints/basispoints"
)

type hostClient struct {
	callbackID string
}

type hostEnvelope struct {
	OK     bool             `json:"ok"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *pluginabi.Error `json:"error,omitempty"`
}

type hostHTTPCall struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
	WireProfile    *wireProfile        `json:"wire_profile,omitempty"`
}

type wireProfile struct {
	HeaderProfile []string `json:"header_profile,omitempty"`
}

type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

type hostHTTPStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	StreamID   string              `json:"stream_id"`
}

type hostStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

func (h hostClient) ListAuth(ctx context.Context) ([]basispoints.AuthRecord, error) {
	raw, errCall := callHost(ctx, pluginabi.MethodHostAuthList, map[string]any{"host_callback_id": h.callbackID})
	if errCall != nil {
		return nil, errCall
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	records := make([]basispoints.AuthRecord, 0, len(resp.Files))
	for _, file := range resp.Files {
		records = append(records, basispoints.AuthRecord{
			AuthIndex:      file.AuthIndex,
			Provider:       file.Provider,
			Type:           file.Type,
			Disabled:       file.Disabled,
			Unavailable:    file.Unavailable,
			NextRetryAfter: file.NextRetryAfter,
		})
	}
	return records, nil
}

func (h hostClient) GetAuth(ctx context.Context, authIndex string) ([]byte, error) {
	raw, errCall := callHost(ctx, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if errCall != nil {
		return nil, errCall
	}
	var resp pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return append([]byte(nil), resp.JSON...), nil
}

func (h hostClient) Do(ctx context.Context, req basispoints.UpstreamRequest) (basispoints.UpstreamResponse, error) {
	raw, errCall := callHost(ctx, pluginabi.MethodHostHTTPDo, h.httpCall(req))
	if errCall != nil {
		return basispoints.UpstreamResponse{}, errCall
	}
	var resp hostHTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return basispoints.UpstreamResponse{}, errUnmarshal
	}
	return basispoints.UpstreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

func (h hostClient) DoStream(ctx context.Context, req basispoints.UpstreamRequest) (basispoints.UpstreamStream, error) {
	raw, errCall := callHost(ctx, pluginabi.MethodHostHTTPDoStream, h.httpCall(req))
	if errCall != nil {
		return basispoints.UpstreamStream{}, errCall
	}
	var opened hostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &opened); errUnmarshal != nil {
		return basispoints.UpstreamStream{}, errUnmarshal
	}
	return basispoints.UpstreamStream{
		StatusCode: opened.StatusCode,
		Headers:    opened.Headers,
		Stream:     &hostPullStream{callbackID: h.callbackID, streamID: opened.StreamID},
	}, nil
}

func (h hostClient) Emit(ctx context.Context, streamID string, payload []byte) error {
	_, errCall := callHost(ctx, pluginabi.MethodHostStreamEmit, map[string]any{
		"host_callback_id": h.callbackID,
		"stream_id":        streamID,
		"payload":          append([]byte(nil), payload...),
	})
	return errCall
}

func (h hostClient) CloseStream(ctx context.Context, streamID, errMsg string) error {
	_, errCall := callHost(ctx, pluginabi.MethodHostStreamClose, map[string]any{
		"host_callback_id": h.callbackID,
		"stream_id":        streamID,
		"error":            errMsg,
	})
	return errCall
}

func (h hostClient) Log(level, message string, fields map[string]string) {
	copied := map[string]any{}
	for key, value := range fields {
		copied[key] = value
	}
	_, _ = callHost(context.Background(), pluginabi.MethodHostLog, map[string]any{
		"host_callback_id": h.callbackID,
		"level":            level,
		"message":          message,
		"fields":           copied,
	})
}

func (h hostClient) httpCall(req basispoints.UpstreamRequest) hostHTTPCall {
	call := hostHTTPCall{
		HostCallbackID: h.callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
	}
	if len(req.HeaderProfile) > 0 {
		call.WireProfile = &wireProfile{HeaderProfile: req.HeaderProfile}
	}
	return call
}

type hostPullStream struct {
	callbackID string
	streamID   string
	closed     bool
}

func (s *hostPullStream) Read() ([]byte, error) {
	if s == nil || s.closed {
		return nil, io.EOF
	}
	raw, errCall := callHost(context.Background(), pluginabi.MethodHostHTTPStreamRead, map[string]string{
		"host_callback_id": s.callbackID,
		"stream_id":        s.streamID,
	})
	if errCall != nil {
		return nil, errCall
	}
	var resp hostStreamReadResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if resp.Error != "" {
		return resp.Payload, fmt.Errorf("%s", resp.Error)
	}
	if resp.Done {
		if len(resp.Payload) == 0 {
			return nil, io.EOF
		}
		return resp.Payload, io.EOF
	}
	return resp.Payload, nil
}

func (s *hostPullStream) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	_, errCall := callHost(context.Background(), pluginabi.MethodHostHTTPStreamClose, map[string]string{
		"host_callback_id": s.callbackID,
		"stream_id":        s.streamID,
	})
	return errCall
}

func callHost(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	_ = ctx
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}
	rawResponse, callCode, errCall := callHostRaw(method, rawPayload)
	if errCall != nil {
		return nil, fmt.Errorf("host callback %s: %w", method, errCall)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, callCode)
	}
	var env hostEnvelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil && env.Error.Message != "" {
			return nil, fmt.Errorf("%s", env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, callCode)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}
