package bridge

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// Reject recognizable credentials instead of silently editing user content.
// This is a narrow text guard, not PII detection or OCR. Values never enter
// errors; matching happens before a run is claimed or callbacks are released.
var interactiveCredentialPattern = regexp.MustCompile(`sk-ant-(?:api[0-9]+|oat[0-9]+|ort[0-9]+)-[A-Za-z0-9_-]{20,}|\bgh[pousr]_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{40,}\b|-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----`)

func validateInteractiveOutboundCredentials(body []byte, headers http.Header) error {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return err
	}
	var callerKeys []string
	for _, key := range []string{headers.Get("x-api-key"), strings.TrimPrefix(headers.Get("Authorization"), "Bearer ")} {
		if len(key) >= 16 {
			callerKeys = append(callerKeys, key)
		}
	}
	sensitive := func(text string) bool {
		if interactiveCredentialPattern.MatchString(text) {
			return true
		}
		for _, key := range callerKeys {
			if strings.Contains(text, key) {
				return true
			}
		}
		return false
	}
	var visit func(any) bool
	visit = func(v any) bool {
		switch x := v.(type) {
		case string:
			return sensitive(x)
		case []any:
			for _, item := range x {
				if visit(item) {
					return true
				}
			}
		case map[string]any:
			for key, item := range x {
				if sensitive(key) || visit(item) {
					return true
				}
			}
		}
		return false
	}
	if visit(value) {
		return errors.New("Claude interactive privacy guard: request contains a recognizable credential; remove it before sending")
	}
	return nil
}
