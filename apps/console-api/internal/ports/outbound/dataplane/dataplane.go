// Package dataplane is the console-api's outbound port to the Data Plane's
// management surface: the one seam between the two planes, and the only place
// the Control Plane asks the Data Plane to change something the Data Plane
// owns.
//
// It is a port in this module rather than a shared package on purpose. The two
// planes are separate Go modules (ADR 0006 §1), so there is no import path
// through which the Control Plane could reach a Data Plane type; what crosses
// the line is a call over an interface declared here, implemented in
// internal/adapters/outbound by an adapter that speaks the Data Plane's
// management contract. Which transport that adapter uses is its business, and
// the day it changes, this package does not.
//
// The directions are not symmetric, and the asymmetry is the architecture:
// configuration and credential decisions flow Control → Data (this port),
// while facts flow Data → Control (usage events, which the Control Plane
// reconciles from). Credit, quota and subscription state travel as grants, not
// as shared tables, because neither plane may read the other's database
// (ADR 0006 §5, §7).
//
// Exactly one operation is enumerated here, because it is the one the
// architecture already fixes end to end: a credential the Control Plane has
// withdrawn must stop being accepted at the runtime, within a bounded
// staleness window rather than the moment the withdrawal commits (ADR 0006
// §8). The other operations a management surface will eventually need — a
// grant, a capacity publication, a cache invalidation — are not declared here
// as guesses: each arrives with the use case that has to make it, and a port
// method with no caller is a shape invented twice.
//
// No adapter implements this yet, and nothing constructs it: console-api has
// no use case that reaches the Data Plane until the API-key lifecycle lands.
// The port exists now because the seam has to exist before either side of it
// is built, and because its absence would leave a future author with no
// recorded answer to "how does the Control Plane talk to the Data Plane?".
package dataplane

import "context"

// Management is what the Control Plane may ask of the Data Plane.
//
// It is deliberately an interface at the application boundary rather than a
// concrete client: application code calls this, and only the composition root
// chooses what answers behind it.
type Management interface {
	// WithdrawCredential tells the Data Plane that the credential identified
	// by credentialID must no longer authenticate a runtime request.
	//
	// The call is a notification, not a transaction: it can fail, be retried,
	// or arrive after the runtime has already served a request with the
	// credential, and the contract is bounded staleness rather than linear
	// consistency. An implementation reports a transport failure as an error
	// and must not treat a failed withdrawal as a completed one — the Control
	// Plane keeps the credential's revocation pending until the Data Plane has
	// acknowledged it.
	WithdrawCredential(ctx context.Context, credentialID string) error
}
