// Package catalog is the model catalog domain: the aliases clients send by
// name, the ordered candidate lists that resolve them, the backends those
// candidates call, and the immutable alias-group versions entitlement scope
// is denominated in (ADR 0001's Catalog context; ADR 0002's routing
// vocabulary; ADR 0003's group versions).
//
// The package is pure: no driver, no HTTP, no clock, no I/O of any kind. The
// aggregates carry their own validation and their own state machines, the
// constructors return errors rather than panicking on rule violations, and
// every time a rule names a moment, the caller supplies the clock. The
// persistence port that stores these aggregates lives in
// internal/ports/outbound/persistence, and the use cases that drive both in
// internal/application — the dependency arrow points inward, and nothing
// here could import an adapter without the architecture tests failing.
//
// What the catalog deliberately is not: a routing engine. Position order is
// recorded and nothing more — weights, cooldowns, health policies and every
// other future selection policy are add-ons that will read this
// configuration as data (docs/architecture/routing.md), not state this
// package computes. The runtime's request path resolves an alias against
// these rows; this package only says what a well-formed alias, candidate
// list, backend and group version are.
//
// Plane placement is worth one paragraph because the task vocabulary that
// commissioned this package said otherwise: the catalog is the Data Plane's
// own data. The rows land in the `dataplane` database (ADR 0006 §5, §7),
// administered through the management surface, and the Control Plane holds
// no copy of any of them — entitlements reference a group version by id
// alone, across the plane boundary, which is exactly why the versions are
// immutable snapshots and why their ids are UUIDv7 (persistence.md's rule
// for any id that crosses a plane; the identity domain's UUIDv4 is its
// recorded exception, pinned by the token grammar).
package catalog
