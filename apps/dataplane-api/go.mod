// The Data Plane management API: the Data Plane's administrative surface,
// reachable from the Control Plane and from nobody else.
//
// The module path is spelled in full because Go's import path IS the name;
// living under apps/dataplane-api of the ecoma-io/llm-gateway monorepo is the
// accepted cost of the monorepo decision — the same arrangement every module
// in this organisation carries. The path is what makes the plane boundary
// mechanical: neither the runtime nor the Control Plane can import this
// module's internals, and this module requires neither of them.
//
// It has no dependencies at all, and that is a statement rather than an
// accident. A management transport owns no data: it is not on the request
// path, it holds no state of its own, and everything it will eventually
// answer must come from the Data Plane it manages. The day this module needs
// a database driver is the day the boundary has been crossed — see ADR 0006
// §9, which records how this application reaches Data Plane state as the one
// open question of the split.
module github.com/ecoma-io/llm-gateway/apps/dataplane-api

go 1.26
