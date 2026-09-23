package http

import (
	"bytes"
	"errors"
	"io"
	"log"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// The status body every health endpoint returns, exactly as
// api/openapi/runtime.yaml documents it — trailing newline included.
const statusBody = "{\"status\":\"ok\"}\n"

func TestServerServesTheContractedRoutes(t *testing.T) {
	handler := New(newTestApp(t, "v0.1.0"))
	tests := []struct {
		name       string
		method     string
		path       string
		requestID  string
		wantStatus int
		wantAllow  string
		wantBody   string
	}{
		{
			name:       "healthz reports ok",
			method:     stdhttp.MethodGet,
			path:       "/healthz",
			requestID:  "health-request",
			wantStatus: stdhttp.StatusOK,
			wantBody:   statusBody,
		},
		{
			name:       "readyz reports ok",
			method:     stdhttp.MethodGet,
			path:       "/readyz",
			requestID:  "ready-request",
			wantStatus: stdhttp.StatusOK,
			wantBody:   statusBody,
		},
		{
			name:       "version reports the build version handed to the application",
			method:     stdhttp.MethodGet,
			path:       "/version",
			requestID:  "version-request",
			wantStatus: stdhttp.StatusOK,
			wantBody:   "{\"version\":\"v0.1.0\"}\n",
		},
		{
			name:       "an unknown path returns the runtime error body",
			method:     stdhttp.MethodGet,
			path:       "/nope",
			requestID:  "missing-request",
			wantStatus: stdhttp.StatusNotFound,
			wantBody:   notFoundBody,
		},
		{
			name:       "a non-canonical path returns the runtime error body instead of a redirect",
			method:     stdhttp.MethodGet,
			path:       "http://example.com//version",
			requestID:  "double-slash-request",
			wantStatus: stdhttp.StatusNotFound,
			wantBody:   notFoundBody,
		},
		{
			name:       "healthz refuses a non-GET method with the runtime error body",
			method:     stdhttp.MethodPost,
			path:       "/healthz",
			requestID:  "method-request",
			wantStatus: stdhttp.StatusMethodNotAllowed,
			wantAllow:  "GET, HEAD",
			wantBody:   methodNotAllowedBody,
		},
		{
			// The exact bytes api/openapi/runtime.yaml contracts for the one
			// operation on this surface. A 404 would be the wrong answer here
			// and a plausible one — the path would look unrouted — which is
			// why the status is asserted rather than only the envelope.
			name:       "the contracted inference endpoint reports not implemented",
			method:     stdhttp.MethodPost,
			path:       "/v1/chat/completions",
			requestID:  "inference-request",
			wantStatus: stdhttp.StatusNotImplemented,
			wantBody:   notImplementedBody,
		},
		{
			name:       "the inference endpoint refuses the methods it does not accept",
			method:     stdhttp.MethodGet,
			path:       "/v1/chat/completions",
			requestID:  "inference-method-request",
			wantStatus: stdhttp.StatusMethodNotAllowed,
			wantAllow:  "POST",
			wantBody:   methodNotAllowedBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set(RequestIDHeader, tt.requestID)
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("%s %s: status = %d, want %d", tt.method, tt.path, rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get(RequestIDHeader); got != tt.requestID {
				t.Errorf("%s %s: response %s = %q, want %q", tt.method, tt.path, RequestIDHeader, got, tt.requestID)
			}
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("%s %s: Allow = %q, want %q", tt.method, tt.path, got, tt.wantAllow)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("%s %s: body = %q, want %q", tt.method, tt.path, got, tt.wantBody)
			}
		})
	}
}

func TestApplicationErrorsMapToSafeHTTPResponses(t *testing.T) {
	// A cause a client must never see. The mapping's job is to keep it on the
	// server side of the boundary.
	secret := "postgres://user:super-secret@database.example/gateway"
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "a not found application error keeps its client-safe message",
			err:        application.NotFound("the requested version does not exist"),
			wantStatus: stdhttp.StatusNotFound,
			wantBody:   "{\"error\":{\"message\":\"the requested version does not exist\",\"type\":\"not_found_error\",\"param\":null,\"code\":null}}\n",
		},
		{
			name:       "an internal application error hides its cause",
			err:        application.Internal(errors.New(secret)),
			wantStatus: stdhttp.StatusInternalServerError,
			wantBody:   internalErrorBody,
		},
		{
			name:       "an unknown application code normalizes to internal",
			err:        &application.Error{Code: application.Code("unexpected"), Message: secret},
			wantStatus: stdhttp.StatusInternalServerError,
			wantBody:   internalErrorBody,
		},
		{
			name:       "an error that is not an application error normalizes to internal",
			err:        errors.New(secret),
			wantStatus: stdhttp.StatusInternalServerError,
			wantBody:   internalErrorBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, "/version", nil)
			req = req.WithContext(withRequestID(req.Context(), "error-request"))

			writeError(rec, req, tt.err)

			if rec.Code != tt.wantStatus {
				t.Errorf("writeError() status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("writeError() body = %q, want %q", got, tt.wantBody)
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("writeError() leaked an internal detail: body = %q", rec.Body.String())
			}
		})
	}
}

func TestTheHandlerChainKeepsStreamingInterfacesReachable(t *testing.T) {
	// New() composes requestID around a mux whose routes come from routes.go;
	// this probe serves those exact pieces over a real socket, because an
	// httptest.ResponseRecorder never implements Hijacker and would therefore
	// hide a wrapper that strips it. Any future ResponseWriter wrapper — the
	// failure mode that would silently break streaming responses — turns both
	// assertions red.
	mux := stdhttp.NewServeMux()
	register(mux, route{
		method: stdhttp.MethodGet,
		path:   "/probe",
		handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
			flusher, flushes := w.(stdhttp.Flusher)
			if !flushes {
				t.Error("the writer a route receives is not an http.Flusher")
			}
			if _, hijacks := w.(stdhttp.Hijacker); !hijacks {
				t.Error("the writer a route receives is not an http.Hijacker")
			}
			_, _ = w.Write([]byte("first\n"))
			if flushes {
				flusher.Flush()
			}
		},
	})
	server := httptest.NewServer(requestID(mux))
	t.Cleanup(server.Close)

	response, err := stdhttp.Get(server.URL + "/probe")
	if err != nil {
		t.Fatalf("GET /probe error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the probe response body error = %v", err)
	}
	if !strings.Contains(string(body), "first") {
		t.Errorf("flushed chunk missing from the probe body = %q", body)
	}
}

func TestInternalErrorsLogOnlyTheCorrelationFact(t *testing.T) {
	// The envelope keeps a cause away from the client; this pins the same
	// discipline on the server's own log line, which would otherwise print
	// whatever an operational failure happens to embed.
	secret := "postgres://user:super-secret@database.example/gateway"

	var logs bytes.Buffer
	writer, flags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(writer)
		log.SetFlags(flags)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, "/version", nil)
	req = req.WithContext(withRequestID(req.Context(), "log-request"))
	writeError(rec, req, application.Internal(errors.New(secret)))

	if strings.Contains(logs.String(), secret) {
		t.Errorf("the internal error log leaked its cause: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "log-request") {
		t.Errorf("the internal error log lost the request ID: %q", logs.String())
	}
}
