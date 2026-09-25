package openaicompatible

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
)

// readBounded reads r whole and refuses a body longer than limit — the
// bounded read every provider-controlled byte stream here passes through,
// so a hostile or broken upstream spends the call's memory, not the
// caller's patience. The overlong case is its own error: the content is
// dropped unread past the cap, never buffered in full.
func readBounded(r io.Reader, limit int) ([]byte, error) {
	reader := io.LimitReader(r, int64(limit)+1)
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errBodyOverLimit
	}
	return raw, nil
}

// errBodyOverLimit marks a body that outgrew its read bound — distinct from
// any transport error, so a caller can tell a hostile body from a cut one.
var errBodyOverLimit = errors.New("openaicompatible: the body outgrew its read bound")

// classifyFailure maps one refused call's status — and, for the request
// faults, the body that travelled with them — onto execution's closed error
// vocabulary. The order is the vocabulary's own precedence: a rate limit is
// a rate limit whatever else the envelope says, the authentication statuses
// are the two the convention reserves, and a request fault is context-sized
// only when the provider said so in words.
func classifyFailure(status int, envelope []byte) execution.ErrorClass {
	switch {
	case status == 429:
		return execution.ErrorRateLimited
	case status == 401 || status == 403:
		return execution.ErrorAuthentication
	case status == 413:
		return execution.ErrorContextTooLarge
	case status >= 500:
		return execution.ErrorUpstreamError
	case status >= 400:
		if contextShaped(envelope) {
			return execution.ErrorContextTooLarge
		}
		return execution.ErrorProviderRejectedRequest
	default:
		// The 1xx and 3xx ranges: a proxy's interjection or a redirect
		// this client refuses to follow. The endpoint did not answer as
		// a provider; the dial, not the request, is what failed.
		return execution.ErrorProviderUnavailable
	}
}

// contextShaped reports whether a refusal body speaks of context size — the
// one request fault the vocabulary separates from the rest, because its
// disposition is different: no other candidate of the same alias will be
// bigger-hearted, and surfacing the refusal beats burning the walk on
// candidates doomed to repeat it. The markers are the spellings the
// OpenAI-compatible ecosystem actually emits.
func contextShaped(envelope []byte) bool {
	if len(envelope) == 0 {
		return false
	}
	lowered := strings.ToLower(string(envelope))
	for _, marker := range []string{"context length", "context_length", "context window", "maximum context"} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// redactedEnvelope renders a refusal body as the jsonb telemetry the attempt
// row keeps: an object, stripped of every auth-shaped key and every value
// that is itself credential-shaped, capped at the domain's own bound — an
// envelope that survives redaction only to overflow the cap is dropped
// whole, because failing a call's classification over debugging telemetry
// would be the tail wagging the attempt.
//
// The strip's honest boundary: a credential quoted inside a prose message
// survives, because a message is content this gateway does not rewrite. What
// cannot survive is structure — a key that names auth material is gone, and
// a string that is nothing but token-shaped material is gone with it.
func redactedEnvelope(body []byte) json.RawMessage {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Not JSON: nothing here can be key-stripped, so nothing here is
		// safe to keep. The class carries the fault; the envelope was
		// never the load-bearing part.
		return nil
	}
	redacted, ok := redactValue(parsed).(map[string]any)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(redacted)
	if err != nil || len(encoded) > execution.MaxProviderErrorOctets {
		return nil
	}
	return encoded
}

// redactValue walks parsed JSON and strips auth material: every key the
// auth-shaped test names, and every string value that is itself
// credential-shaped. Containers are rebuilt so the original is never
// half-redacted in place.
func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		kept := make(map[string]any, len(typed))
		for key, inner := range typed {
			if authShapedKey(key) {
				continue
			}
			kept[key] = redactValue(inner)
		}
		return kept
	case []any:
		kept := make([]any, len(typed))
		for i, inner := range typed {
			kept[i] = redactValue(inner)
		}
		return kept
	case string:
		if credentialShapedValue(typed) {
			return "[stripped]"
		}
		return typed
	default:
		return typed
	}
}

// authShapedKey reports whether an object's key names credential material.
// The test is deliberately over-broad — it matches substrings after
// normalising the separators, so "max_tokens" falls too — because this is
// telemetry, not data: an over-stripped envelope costs a debugging fact, an
// under-stripped one costs a secret in the attempt row.
func authShapedKey(key string) bool {
	normalised := strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(strings.ToLower(key))
	for _, marker := range []string{
		"authorization", "auth", "apikey", "api_key", "key", "token", "secret",
		"password", "credential", "session", "cookie", "bearer", "signature",
	} {
		if strings.Contains(normalised, marker) {
			return true
		}
	}
	return false
}

// credentialShapedValue reports whether a string value is itself nothing but
// credential material: a bearer scheme, or a bare token of the length and
// character set credentials come in. Prose has spaces; this test does not
// match prose.
func credentialShapedValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) == 0 {
		return false
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "bearer ") {
		return true
	}
	if len(trimmed) < 32 {
		return false
	}
	for _, r := range trimmed {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}
