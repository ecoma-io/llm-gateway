// Package identity is the Control Plane's ownership root: the Account,
// User and API-key aggregates of ADR 0001, the principal an authenticated
// caller becomes, and the key-material pipeline a credential must survive.
//
// The package is deliberately framework-independent. It imports nothing from
// net/http (no bearer parsing), nothing from any SQL driver (no row structs),
// and owns no clock: every constructor and transition takes its instant as a
// parameter, so the lifecycle machines here are fully deterministic under
// test. Reading and writing these aggregates is the outbound persistence
// port's job (internal/ports/outbound/persistence); turning a presented
// token into a Principal is VerifyCredential, and the later phases that call
// it — the Data Plane's hot path in particular — mirror the same pipeline
// against their own credential record (ADR 0006 §8).
//
// Two invariants shape everything below:
//
//   - An API key is two records in two planes (ADR 0006 §8). The aggregates
//     here are the ownership record: who the key belongs to and whether it
//     has been revoked. The secret's digest is deliberately NOT stored in
//     the Control Plane; it travels only through the values in this package
//     and lives, at rest, in the Data Plane's credential record.
//   - Lifecycle states are one-way where the ADRs say so. A closed account
//     never reopens, a removed user never returns, and a revoked key is
//     never un-revoked. Deletion does not exist; every exit is a state.
package identity
