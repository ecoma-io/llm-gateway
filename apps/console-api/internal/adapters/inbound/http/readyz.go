package http

import (
	"context"
	"log"
	stdhttp "net/http"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// readinessPingTimeout bounds the one question /readyz asks: five seconds,
// the same budget startup gives its single dial (postgresConnectTimeout in
// cmd/console-api), because it is the same question — can this pool produce
// a working connection — asked on the way out instead of on the way in. The
// runtime's probe rides its request context alone, bounded there by the
// listener's read timeout; this listener has no such bound yet, so the probe
// states its own, and a lost database answers 503 after five seconds rather
// than holding the probe open until some orchestrator's timeout answers for
// it.
const readinessPingTimeout = 5 * time.Second

// readyz gates the readiness probe on the Control Plane's own dependency, and
// answers with the shared status body only while it is up.
//
// The one check is the thing this process cannot serve the console without:
// its database answering. The Data Plane is never among the dependencies —
// this service is ready when it can serve the console, and it can do that
// while the runtime is down — and the cache joins the gate when a caller
// first reads through one. Liveness stays ungated on /healthz, so an
// orchestrator restarting on the process probe never kills this process for
// a dependency it is on its way to reach; this is the endpoint traffic
// control reads, and 503 here is the service saying "not yet", not "never".
//
// A failed check logs exactly one line naming the dependency and nothing else
// about it — no driver prose, no stack — because the envelope already told
// the caller everything it can act on, and the request identifier is what
// joins the two.
func readyz(readiness persistence.Pinger) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), readinessPingTimeout)
		defer cancel()
		if err := readiness.Ping(pingCtx); err != nil {
			notReady(w, r, "database")
			return
		}
		writeStatus(w)
	}
}

// notReady answers 503 with the console error envelope and logs which
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
