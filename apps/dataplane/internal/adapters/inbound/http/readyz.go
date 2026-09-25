package http

import (
	"log"
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// readyz gates the readiness probe on the runtime's own dependencies, and
// answers with the shared status body only while every one of them is up.
//
// The two checks are the things this process cannot serve an inference
// request without: its database answering, and the credential projection's
// first snapshot applied — a mirror that never bootstrapped holds no
// credential a request could be admitted against (ADR 0007). Liveness stays
// ungated on /healthz, so an orchestrator restarting on the process probe
// never kills a runtime for a dependency it is on its way to reach; this is
// the endpoint traffic control reads, and 503 here is the runtime saying "not
// yet", not "never".
//
// Each failed check logs exactly one line naming the dependency and nothing
// else about it — no driver prose, no stack — because the envelope already
// told the caller everything it can act on, and the request identifier is
// what joins the two.
func readyz(app *application.App) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if err := app.PingDatabase(r.Context()); err != nil {
			notReady(w, r, "database")
			return
		}
		position, err := app.ProjectionPosition(r.Context())
		if err != nil || !position.Bootstrapped {
			// A position that cannot be read and a position that was never
			// earned are the same refusal on the wire — the mirror is not
			// there to admit against — so both name the same dependency.
			notReady(w, r, "projection")
			return
		}
		writeStatus(w)
	}
}

// notReady answers 503 with the runtime error envelope and logs which
// dependency is missing. The log line is in the landed shape — service name,
// request identifier, key=value — and the dependency name is the whole of the
// operational detail it carries.
func notReady(w stdhttp.ResponseWriter, r *stdhttp.Request, dependency string) {
	// The middleware guarantees an identifier; the empty case is a handler
	// called without it, where writeError mints its own and the two never need
	// to agree.
	requestID, _ := RequestIDFromContext(r.Context())
	log.Printf("%s request_id=%s readyz dependency=%s not ready", serviceName, requestID, dependency)
	writeError(w, r, notReadyError{})
}
