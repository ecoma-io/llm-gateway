package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stdhttp "net/http"
)

const (
	requestIDBytes = 16
	requestIDMax   = 64
)

type requestIDContextKey struct{}

// requestID chooses the one identifier a request carries through the plane.
// It sets the response header before the next handler can commit a status, then
// puts the same safe, bounded value in the request context for handlers and the
// error envelope. A caller gets to preserve a valid identifier, never to make a
// log line carry an arbitrary-sized or control-character value.
func requestID(next stdhttp.Handler) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		id := validIncomingRequestID(r)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
	})
}

func validIncomingRequestID(r *stdhttp.Request) string {
	values := r.Header.Values(RequestIDHeader)
	if len(values) != 1 || !isValidRequestID(values[0]) {
		return ""
	}
	return values[0]
}

func isValidRequestID(value string) bool {
	if len(value) == 0 || len(value) > requestIDMax {
		return false
	}
	for _, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func newRequestID() string {
	bytes := make([]byte, requestIDBytes)
	if _, err := rand.Read(bytes); err != nil {
		// crypto/rand failures are failures of the host's entropy source, not an
		// input a caller can repair, and no fallback can be honest: a constant or
		// time-derived identifier would make correlation lie. The panic costs the
		// calling connection its response — net/http recovers it per connection,
		// so that caller gets nothing at all rather than a response without the
		// request ID the contract promises, while the process keeps serving the
		// requests that still have entropy.
		panic(serviceName + " request ID entropy unavailable")
	}
	return hex.EncodeToString(bytes)
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// RequestIDFromContext returns the request ID installed by requestID. The bool
// distinguishes a gateway handler's context from an unrelated background
// context, which lets code avoid inventing a second request ID on its own.
func RequestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDContextKey{}).(string)
	return id, ok
}
