package fixture

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var forbiddenKey = regexp.MustCompile(`(?i)^(access[_-]?token|refresh[_-]?token|id[_-]?token|authorization|cookie|secret)$`)
var email = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
var uuid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var jwt = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

var sentinels = map[string]bool{"SANITIZED_EMAIL": true, "SANITIZED_USER_ID": true, "SANITIZED_ACCOUNT_ID": true, "SANITIZED_ORG_ID": true, "SANITIZED_NAME": true, "SANITIZED_TIMESTAMP": true, "SANITIZED_ACCESS_TOKEN": true, "SANITIZED_REFRESH_TOKEN": true, "SANITIZED_ID_TOKEN": true}

type Kind string

const (
	Usage Kind = "usage"
	Token Kind = "token"
)

func Sanitize(raw []byte, kind Kind) ([]byte, error) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, errors.New("fixture must be JSON")
	}
	out, err := sanitize(value, kind, "")
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(out, "", "  ")
}
func Scan(raw []byte, kind Kind) error {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return errors.New("fixture must be JSON")
	}
	return scan(value, kind, "")
}

func sanitize(v any, kind Kind, path string) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			key := strings.ToLower(k)
			next := path + "/" + k
			if forbiddenKey.MatchString(k) {
				if kind != Token {
					return nil, fmt.Errorf("credential key at %s", next)
				}
				switch key {
				case "access_token", "accesstoken":
					out[k] = "SANITIZED_ACCESS_TOKEN"
				case "refresh_token", "refreshtoken":
					out[k] = "SANITIZED_REFRESH_TOKEN"
				case "id_token", "idtoken":
					out[k] = "SANITIZED_ID_TOKEN"
				default:
					return nil, fmt.Errorf("undeclared credential key at %s", next)
				}
				continue
			}
			if isIdentityKey(key) {
				out[k] = sentinelFor(key)
				continue
			}
			y, err := sanitize(v, kind, next)
			if err != nil {
				return nil, err
			}
			out[k] = y
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			y, err := sanitize(v, kind, fmt.Sprintf("%s/%d", path, i))
			if err != nil {
				return nil, err
			}
			out[i] = y
		}
		return out, nil
	case string:
		if jwt.MatchString(x) || email.MatchString(x) || uuid.MatchString(x) {
			return nil, fmt.Errorf("identity or credential value at %s", path)
		}
		return x, nil
	default:
		return v, nil
	}
}
func scan(v any, kind Kind, path string) error {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			next := path + "/" + k
			if forbiddenKey.MatchString(k) {
				allowed := kind == Token && ((strings.Contains(strings.ToLower(k), "access") && v == "SANITIZED_ACCESS_TOKEN") || (strings.Contains(strings.ToLower(k), "refresh") && v == "SANITIZED_REFRESH_TOKEN") || (strings.Contains(strings.ToLower(k), "id_") && v == "SANITIZED_ID_TOKEN"))
				if !allowed {
					return fmt.Errorf("unsafe credential key at %s", next)
				}
			}
			if err := scan(v, kind, next); err != nil {
				return err
			}
		}
	case []any:
		for i, v := range x {
			if err := scan(v, kind, fmt.Sprintf("%s/%d", path, i)); err != nil {
				return err
			}
		}
	case string:
		if sentinels[x] {
			return nil
		}
		if jwt.MatchString(x) || email.MatchString(x) || uuid.MatchString(x) {
			return fmt.Errorf("unsafe value at %s", path)
		}
	}
	return nil
}
func isIdentityKey(k string) bool {
	return strings.Contains(k, "email") || strings.Contains(k, "user_id") || strings.Contains(k, "account_id") || strings.Contains(k, "organization_id") || k == "name" || strings.Contains(k, "timestamp") || strings.HasSuffix(k, "_at")
}
func sentinelFor(k string) string {
	switch {
	case strings.Contains(k, "email"):
		return "SANITIZED_EMAIL"
	case strings.Contains(k, "account"):
		return "SANITIZED_ACCOUNT_ID"
	case strings.Contains(k, "organization"):
		return "SANITIZED_ORG_ID"
	case strings.Contains(k, "timestamp") || strings.HasSuffix(k, "_at"):
		return "SANITIZED_TIMESTAMP"
	case k == "name":
		return "SANITIZED_NAME"
	default:
		return "SANITIZED_USER_ID"
	}
}
