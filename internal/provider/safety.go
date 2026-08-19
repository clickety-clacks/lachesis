package provider

import (
	"encoding/json"
	"strings"
)

var forbiddenRawKeys = map[string]bool{"access_token": true, "accesstoken": true, "refreshtoken": true, "refresh_token": true, "id_token": true, "idtoken": true, "authorization": true, "cookie": true, "secret": true}

func RawUsageSafe(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return safeValue(value)
}
func safeValue(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			key := strings.ToLower(k)
			if forbiddenRawKeys[key] {
				return false
			}
			if !safeValue(v) {
				return false
			}
		}
	case []any:
		for _, v := range x {
			if !safeValue(v) {
				return false
			}
		}
	}
	return true
}
