package http

import (
	"bytes"
	"errors"
	"log"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// notReadyBody is the exact bytes api/openapi/runtime.yaml contracts for the
// readiness probe's one refusal, spelled out rather than built from the
// production struct for the same reason its siblings in wireerrors_test.go
// are: the wire shape is the point, and `param` and `code` being present and
// null is part of it.
const notReadyBody = "{\"error\":{\"message\":\"the runtime is not ready to serve requests\",\"type\":\"overloaded_error\",\"param\":null,\"code\":null}}\n"

// TestReadyzGatesOnTheRuntimeOwnDependencies drives the probe through every
// answer its two dependencies can give. A 503 is only ever the runtime saying
// "not yet" about itself — never about the Control Plane, which is nobody's
// dependency here (ADR 0006 §4) — so the cases are exactly: both dependencies
// up, either one down.
func TestReadyzGatesOnTheRuntimeOwnDependencies(t *testing.T) {
	tests := []struct {
		name        string
		store       silentStore
		projections stubProjections
		wantStatus  int
		wantBody    string
	}{
		{
			name:        "a store that answers and a bootstrapped projection are ready",
			store:       silentStore{t: t},
			projections: stubProjections{t: t, position: readyPosition},
			wantStatus:  stdhttp.StatusOK,
			wantBody:    statusBody,
		},
		{
			name:        "a store that does not answer is not ready",
			store:       silentStore{t: t, pingErr: errors.New("ping: connection refused")},
			projections: stubProjections{t: t, position: readyPosition},
			wantStatus:  stdhttp.StatusServiceUnavailable,
			wantBody:    notReadyBody,
		},
		{
			name:        "a projection that never applied a snapshot is not ready",
			store:       silentStore{t: t},
			projections: stubProjections{t: t},
			wantStatus:  stdhttp.StatusServiceUnavailable,
			wantBody:    notReadyBody,
		},
		{
			name:        "a projection position that cannot be read is not ready",
			store:       silentStore{t: t},
			projections: stubProjections{t: t, positionErr: errors.New("read projection position: connection refused")},
			wantStatus:  stdhttp.StatusServiceUnavailable,
			wantBody:    notReadyBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := New(newTestAppWith(t, "test", tt.store, tt.projections))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, "/readyz", nil)
			req.Header.Set(RequestIDHeader, "readyz-request")
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("GET /readyz: status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get(RequestIDHeader); got != "readyz-request" {
				t.Errorf("GET /readyz: response %s = %q, want %q", RequestIDHeader, got, "readyz-request")
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("GET /readyz: Content-Type = %q, want application/json", got)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("GET /readyz: body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

// TestReadyzLogsTheDependencyAndNothingElse pins the 503 path's one log line:
// which dependency is missing, under the request identifier, and nothing of
// the cause. The driver's own message is an operational detail — sometimes a
// credential-carrying one — and a shared log stream is exactly where it must
// not land.
func TestReadyzLogsTheDependencyAndNothingElse(t *testing.T) {
	secret := "postgres://user:super-secret@database.example/gateway"

	var logs bytes.Buffer
	writer, flags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(writer)
		log.SetFlags(flags)
	})

	handler := New(newTestAppWith(t, "test",
		silentStore{t: t, pingErr: errors.New("ping: " + secret)},
		stubProjections{t: t, position: readyPosition}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, "/readyz", nil)
	// Through New the middleware chooses the identifier; a valid header it
	// preserves, so the log line is asserted against the same value the
	// response header carries.
	req.Header.Set(RequestIDHeader, "readyz-log")
	handler.ServeHTTP(rec, req)

	if rec.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("GET /readyz: status = %d, want %d", rec.Code, stdhttp.StatusServiceUnavailable)
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("the readiness log leaked its cause: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "dependency=database") {
		t.Errorf("the readiness log lost the dependency that failed: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "readyz-log") {
		t.Errorf("the readiness log lost the request ID: %q", logs.String())
	}
}
