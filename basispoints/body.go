package basispoints

import "strings"

var upstreamHeaderProfile = []string{
	"authorization",
	"content-type",
	"accept",
	"chatgpt-account-id",
	"x-openai-account-id",
	"x-basispoints-auth-mode",
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
