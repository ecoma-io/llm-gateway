// Package server is the HTTP surface of the Ecoma LLM Gateway.
//
// Product endpoints do not exist yet: every route this package serves today
// is infrastructure (health, readiness), and every route it will serve later
// arrives through the OpenAPI contract at api/openapi/openapi.yaml — the
// contract is changed first, the handler second.
package server

import "net/http"

// New returns the gateway's HTTP handler.
//
// The mux's method patterns ("GET /healthz") mean the method is part of the
// route: a POST to /healthz is a 405 from the mux itself, not a handler that
// has to remember to check r.Method.
func New() http.Handler {
	mux := http.NewServeMux()

	// Liveness: the process is up and its loop is turning. Anything about
	// whether the gateway could do useful work — a dependency reachable, a
	// cache warm — is readiness's job and never appears here, so an
	// orchestrator restarting on /healthz never kills a process for a
	// downstream outage it cannot fix.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w)
	})

	// Readiness: the scaffold has no dependencies, so it is always ready.
	// The checks that will gate this endpoint later — a database ping, an
	// upstream probe — hang off here, and only here.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w)
	})

	return mux
}

// writeStatus is the one definition of what a health endpoint's body looks
// like, shared by /healthz and /readyz so the two cannot drift apart; the
// shape it writes is the one api/openapi/openapi.yaml documents.
func writeStatus(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}
