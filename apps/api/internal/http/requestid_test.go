package http

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

var requestIDPattern = regexp.MustCompile("^[A-Za-z0-9._-]{1,64}$")

func TestRequestIDMiddlewareChoosesOneSafeIdentifier(t *testing.T) {
	tests := []struct {
		name      string
		header    []string
		wantExact string
	}{
		{
			name: "generates an identifier when the request supplies none",
		},
		{
			name:      "accepts a valid caller supplied identifier",
			header:    []string{"trace.2026_09-23-abc"},
			wantExact: "trace.2026_09-23-abc",
		},
		{
			name:   "replaces an unsafe caller supplied identifier",
			header: []string{"unsafe value with spaces"},
		},
		{
			name:   "replaces ambiguous multiple caller supplied identifiers",
			header: []string{"first", "second"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Every handler call records what the context carried, so a second
			// request cannot silently overwrite the first one's evidence.
			var contextIDs []string
			handler := requestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				id, _ := RequestIDFromContext(r.Context())
				contextIDs = append(contextIDs, id)
			}))

			responseID, recorder := serveWithRequestID(handler, tt.header)
			if !requestIDPattern.MatchString(responseID) {
				t.Errorf("response %s = %q, want a safe bounded identifier", RequestIDHeader, responseID)
			}
			if contextIDs[0] != responseID {
				t.Errorf("context request ID = %q, want response header %q", contextIDs[0], responseID)
			}
			if recorder.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if tt.wantExact != "" && responseID != tt.wantExact {
				t.Errorf("response %s = %q, want %q", RequestIDHeader, responseID, tt.wantExact)
			}
			if len(tt.header) > 0 && tt.wantExact == "" && responseID == tt.header[0] {
				t.Errorf("response %s retained invalid input %q", RequestIDHeader, responseID)
			}

			// A second request must not be handed the first one's identifier: a
			// generator that returned a constant would satisfy every assertion
			// above and correlate nothing.
			secondID, _ := serveWithRequestID(handler, nil)
			if secondID == responseID {
				t.Errorf("two generated request IDs are both %q — generation is not happening", secondID)
			}
			if contextIDs[1] != secondID {
				t.Errorf("second context request ID = %q, want response header %q", contextIDs[1], secondID)
			}
		})
	}
}

func serveWithRequestID(handler http.Handler, header []string) (string, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, value := range header {
		req.Header.Add(RequestIDHeader, value)
	}
	handler.ServeHTTP(rec, req)
	return rec.Header().Get(RequestIDHeader), rec
}
