package basispoints

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var upstreamHeaderProfile = []string{
	"authorization",
	"content-type",
	"accept",
	"chatgpt-account-id",
	"x-openai-account-id",
	"x-basispoints-auth-mode",
}

func prepareBody(body []byte, upstreamModel, suffixEffort, maxEffort string) ([]byte, error) {
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	updated, errSet := sjson.SetBytes(body, "model", upstreamModel)
	if errSet != nil {
		return nil, errSet
	}
	updated, errSet = sjson.SetBytes(updated, "stream", true)
	if errSet != nil {
		return nil, errSet
	}
	updated, errSet = sjson.DeleteBytes(updated, "previous_response_id")
	if errSet != nil {
		return nil, errSet
	}

	effort := strings.TrimSpace(gjson.GetBytes(updated, "reasoning.effort").String())
	if effort == "" {
		effort = strings.TrimSpace(suffixEffort)
	}
	if strings.EqualFold(effort, "none") {
		updated, errSet = sjson.DeleteBytes(updated, "reasoning.effort")
		if errSet != nil {
			return nil, errSet
		}
		return updated, nil
	}
	clamped := clampEffort(effort, maxEffort)
	if clamped == "" {
		return updated, nil
	}
	updated, errSet = sjson.SetBytes(updated, "reasoning.effort", clamped)
	if errSet != nil {
		return nil, errSet
	}
	return updated, nil
}

func upstreamHeaders(token, accountID, authMode string) map[string][]string {
	return map[string][]string{
		"authorization":           {"Bearer " + token},
		"content-type":            {"application/json"},
		"accept":                  {"text/event-stream"},
		"chatgpt-account-id":      {accountID},
		"x-openai-account-id":     {accountID},
		"x-basispoints-auth-mode": {authMode},
	}
}

func headerValue(headers map[string][]string, name string) string {
	if len(headers) == 0 {
		return ""
	}
	if values := headers[name]; len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}
