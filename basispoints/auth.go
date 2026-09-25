package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

func credentialFromJSON(raw []byte) (string, string, bool) {
	token := firstJSONString(raw, "access_token", "metadata.access_token")
	accountID := firstJSONString(raw, "account_id", "metadata.account_id", "chatgpt_account_id")
	if accountID == "" {
		accountID = accountIDFromJWT(token)
	}
	if accountID == "" {
		accountID = accountIDFromJWT(firstJSONString(raw, "id_token", "metadata.id_token"))
	}
	if token == "" || accountID == "" {
		return "", "", false
	}
	return token, accountID, true
}

func firstJSONString(raw []byte, paths ...string) string {
	for _, path := range paths {
		value := strings.TrimSpace(gjson.GetBytes(raw, path).String())
		if value != "" {
			return value
		}
	}
	return ""
}

func accountIDFromJWT(token string) string {
	payload, ok := jwtPayload(token)
	if !ok {
		return ""
	}
	var claims map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return ""
	}
	if rawAuth, ok := claims["https://api.openai.com/auth"]; ok {
		if id := strings.TrimSpace(gjson.GetBytes(rawAuth, "chatgpt_account_id").String()); id != "" {
			return id
		}
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "chatgpt_account_id").String())
}

func jwtPayload(token string) ([]byte, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil, false
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		payload, errDecode = base64.URLEncoding.DecodeString(parts[1])
		if errDecode != nil {
			return nil, false
		}
	}
	if !json.Valid(payload) {
		return nil, false
	}
	return payload, true
}
