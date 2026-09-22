package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The status body every health endpoint returns, exactly as
// api/openapi/openapi.yaml documents it — trailing newline included.
const statusBody = "{\"status\":\"ok\"}\n"

func TestServer(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "healthz reports ok",
			method:     http.MethodGet,
			path:       "/healthz",
			wantStatus: http.StatusOK,
			wantBody:   statusBody,
		},
		{
			name:       "readyz reports ok",
			method:     http.MethodGet,
			path:       "/readyz",
			wantStatus: http.StatusOK,
			wantBody:   statusBody,
		},
		{
			name:       "an unknown path is not found",
			method:     http.MethodGet,
			path:       "/nope",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "healthz refuses a non-GET method",
			method:     http.MethodPost,
			path:       "/healthz",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			New().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("%s %s: status = %d, want %d", tt.method, tt.path, rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("%s %s: body = %q, want %q", tt.method, tt.path, rec.Body.String(), tt.wantBody)
			}
		})
	}
}
