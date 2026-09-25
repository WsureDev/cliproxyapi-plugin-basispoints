package basispoints

import "strings"

var upstreamHeaderProfile = []string{
	"authorization",
	"content-type",
	"accept",
	"origin",
	"user-agent",
	"chatgpt-account-id",
	"x-openai-account-id",
	"x-basispoints-auth-mode",
	"x-openai-internal-basispoints-client-product",
	"x-openai-internal-basispoints-client-agent-profile",
}

func upstreamHeaders(token, accountID, authMode string) map[string][]string {
	return map[string][]string{
		"authorization":           {"Bearer " + token},
		"content-type":            {"application/json"},
		"accept":                  {"text/event-stream"},
		"origin":                  {"https://bps.openai.com"},
		"user-agent":              {"Mozilla/5.0"},
		"chatgpt-account-id":      {accountID},
		"x-openai-account-id":     {accountID},
		"x-basispoints-auth-mode": {authMode},
		// These two select the Excel agent profile. Without them BPS does not
		// attach native run_officejs, so client tools cannot be transported.
		"x-openai-internal-basispoints-client-product":       {"basispoints-excel-plugin"},
		"x-openai-internal-basispoints-client-agent-profile": {"excel"},
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
