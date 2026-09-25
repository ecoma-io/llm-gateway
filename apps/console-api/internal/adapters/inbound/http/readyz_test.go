package http

import (
	"bytes"
	"context"
	"errors"
	"log"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// notReadyBody is the exact bytes api/openapi/console.yaml contracts for the
// readiness probe's one refusal, spelled out rather than built from the
// production envelope for the same reason its siblings in server_test.go are:
// the wire shape is the point, and the code it carries is part of it.
const notReadyBody = "{\"error\":{\"code\":\"service_unavailable\",\"message\":\"the service is not ready to receive traffic\"},\"request_id\":\"readyz-request\"}\n"

// answeringPinger is the store that answers a readiness probe: nil while the
// database is up, pingErr when the test drives the not-ready path, and the
// context every ping arrived on, so a test can read the deadline the handler
// asked the question under.
type answeringPinger struct {
	pingErr error
	seen    context.Context
}

// Ping implements persistence.Pinger.
func (p *answeringPinger) Ping(ctx context.Context) error {
	p.seen = ctx
	return p.pingErr
}

// Compile-time proof that the fake stands in for the port and not for the
// store as a whole: the probe asks a Pinger, and a test that quietly grew a
// second port member would be testing a shape no production call site has.
var _ persistence.Pinger = (*answeringPinger)(nil)

// TestReadyzGatesOnTheStoreAnswering drives the probe through the two answers
// the Control Plane's own dependency can give. A 503 is only ever this service
// saying "not yet" about itself — never about the Data Plane, which is nobody's
// dependency here (ADR 0006 §4) — so the cases are exactly: the store answers,
// the store does not.
func TestReadyzGatesOnTheStoreAnswering(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "a store that answers is ready",
			wantStatus: stdhttp.StatusOK,
			wantBody:   statusBody,
		},
		{
			name:       "a store that does not answer is not ready",
			pingErr:    errors.New("ping: connection refused"),
			wantStatus: stdhttp.StatusServiceUnavailable,
			wantBody:   notReadyBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := New(application.New("test"), &answeringPinger{pingErr: tt.pingErr})

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

// TestReadyzAsksTheQuestionUnderAShortDeadline pins the timeout the probe
// brings to the store. A readiness check that waits as long as its caller
// allows is a check whose answer arrives whenever the caller gives up, which
// is no answer at all; the deadline below is the statement that the probe
// means to finish, and a lost database turns into a 503 rather than a hang.
func TestReadyzAsksTheQuestionUnderAShortDeadline(t *testing.T) {
	pinger := &answeringPinger{}
	handler := New(application.New("test"), pinger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if pinger.seen == nil {
		t.Fatal("GET /readyz never asked the store whether it was answering")
	}
	deadline, ok := pinger.seen.Deadline()
	if !ok {
		t.Fatal("the readiness ping carried no deadline; a lost database would hold the probe open")
	}
	remaining := time.Until(deadline)
	if remaining > readinessPingTimeout {
		t.Errorf("the readiness ping had %s left, want at most the %s the probe bounds itself with", remaining, readinessPingTimeout)
	}
	// A deadline that has already passed is not a deadline: the probe would
	// refuse a store that is up, so the whole gate would read 503 forever.
	if remaining <= 0 {
		t.Errorf("the readiness ping's deadline had already passed when the store was asked")
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

	handler := New(application.New("test"), &answeringPinger{pingErr: errors.New("ping: " + secret)})

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

// TestHealthzStaysStaticWhileTheStoreIsDown is the liveness/readiness split,
// stated as the failure it prevents: a process whose database is lost must
// stay restartable. If /healthz consulted the store, an orchestrator
// restarting on the process probe would kill this process for a downstream
// outage it cannot fix, and the 503 that would have pulled it from traffic
// would never be read.
func TestHealthzStaysStaticWhileTheStoreIsDown(t *testing.T) {
	handler := New(application.New("test"), &answeringPinger{pingErr: errors.New("connection refused")})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, "/healthz", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != stdhttp.StatusOK {
		t.Errorf("GET /healthz: status = %d, want %d — liveness is the process, not its dependencies", rec.Code, stdhttp.StatusOK)
	}
	if got := rec.Body.String(); got != statusBody {
		t.Errorf("GET /healthz: body = %q, want %q", got, statusBody)
	}
}

// TestNewRefusesAServerWithNothingToGateOn is the loud door: a handler built
// without a store is the static /readyz this change removed, and the only
// honest answer to one is a construction failure rather than a probe that
// reports ready from no evidence at all.
func TestNewRefusesAServerWithNothingToGateOn(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New() built a handler with no readiness Pinger; /readyz would answer ready with nothing behind it")
		}
	}()
	New(application.New("test"), nil)
}
