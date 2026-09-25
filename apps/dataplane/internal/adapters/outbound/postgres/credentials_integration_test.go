//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The credential mirror read against the real `dataplane` database. The
// mirror's write side is pinned by projection_integration_test.go; what only
// PostgreSQL can answer here is the read's own contract: that the single
// LEFT JOINed statement returns the pair of lifecycles as they stood
// together, that a credential whose account row is absent keeps that absence
// as a nil AccountState rather than a synthesized state, and that a miss is
// the ErrCredentialNotFound sentinel — the vocabulary the authentication
// layer burns a constant-time compare on. The mirror is reset to its seeded
// state per test, and the seeding rides the landed applier, exactly as the
// production apply does.

// integrationCredentials hands back a pool, the projection applier the seeds
// ride, and the credential read under test, over a mirror reset to its
// seeded state.
func integrationCredentials(t *testing.T) (*sql.DB, persistence.ProjectionApplier, persistence.Credentials) {
	t.Helper()
	db, applier := integrationProjection(t)
	return db, applier, NewCredentials(New(db))
}

// mustLookup is Lookup with the miss-by-default posture flipped: a test that
// asserts a view fails loudly when the row is not there.
func mustLookup(t *testing.T, creds persistence.Credentials, keyID string) persistence.CredentialView {
	t.Helper()
	view, err := creds.Lookup(t.Context(), keyID)
	if err != nil {
		t.Fatalf("Credentials.Lookup(%s) error = %v", keyID, err)
	}
	return view
}

func TestIntegrationCredentialLookupReturnsTheJoinedPair(t *testing.T) {
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	_, applier, creds := integrationCredentials(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})

	view := mustLookup(t, creds, key)
	if view.Digest != projectionDigestB {
		t.Errorf("digest = %q, want the mirrored 64-hex verbatim", view.Digest)
	}
	if view.AccountID != string(account) {
		t.Errorf("account id = %q, want the credential row's own owner %q — the identity every admission write keys on", view.AccountID, account)
	}
	if view.KeyState != string(projection.CredentialActive) {
		t.Errorf("key state = %q, want active", view.KeyState)
	}
	if view.RevokedAt != nil {
		t.Errorf("revoked_at = %v, want nil on an active key", view.RevokedAt)
	}
	if view.AccountState == nil || *view.AccountState != string(projection.LifecycleActive) {
		t.Errorf("account state = %v, want active — the join carries the account lifecycle beside the key", view.AccountState)
	}
}

func TestIntegrationCredentialLookupKeepsAMissingAccountRowNil(t *testing.T) {
	// The schema carries no foreign key between the mirror's two tables on
	// purpose: rows arrive independently, so a credential can exist while
	// its account row has not (or, after an absence delete, no longer does).
	// The read must surface that absence as a nil AccountState — the
	// integrity-violation case the caller fails closed on — never as an
	// error that would read as a store failure, and never coalesced to a
	// state the mirror does not hold.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	_, applier, creds := integrationCredentials(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 3,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
	})

	view := mustLookup(t, creds, key)
	if view.AccountState != nil {
		t.Errorf("account state = %q, want nil — an absent account row is a fact to fail closed on, not a default", *view.AccountState)
	}
	if view.AccountID != string(account) {
		t.Errorf("account id = %q, want %q — the owner is read from the credential row, so the absence of the account row cannot take it away", view.AccountID, account)
	}
	if view.Digest != projectionDigestB || view.KeyState != string(projection.CredentialActive) {
		t.Errorf("credential = %s/%s, want the row's own halves intact beside the absent account", view.Digest, view.KeyState)
	}
}

func TestIntegrationCredentialLookupCarriesTheRevocation(t *testing.T) {
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	revokedAt := projectionInstant(5)
	_, applier, creds := integrationCredentials(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 7,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialRevoked, RevokedAt: &revokedAt,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})

	view := mustLookup(t, creds, key)
	if view.KeyState != string(projection.CredentialRevoked) {
		t.Errorf("key state = %q, want revoked", view.KeyState)
	}
	if view.RevokedAt == nil || !view.RevokedAt.Equal(revokedAt) {
		t.Errorf("revoked_at = %v, want the mirrored instant %v round-tripped through timestamptz", view.RevokedAt, revokedAt)
	}
}

func TestIntegrationCredentialLookupSeesEachLifecycleAsItLanded(t *testing.T) {
	// The two halves move by independent feed entries, and the read reports
	// each as the mirror holds it: a revoked key beside an active account,
	// then a suspended account beside the revoked key. Neither combination
	// is an error — they are the mirror's honest state, and the caller's
	// verify order decides what each admission does with it.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	_, applier, creds := integrationCredentials(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})
	mustApplyChanges(t, applier, batchFor(epoch,
		credentialChange(t, 11, key, account, projectionDigestB, projection.CredentialRevoked, instantPtr(3)),
	))
	revoked := mustLookup(t, creds, key)
	if revoked.KeyState != string(projection.CredentialRevoked) {
		t.Errorf("key state = %q, want revoked", revoked.KeyState)
	}
	if revoked.AccountState == nil || *revoked.AccountState != string(projection.LifecycleActive) {
		t.Errorf("account state = %v, want active under the revoked key — the halves move independently", revoked.AccountState)
	}

	mustApplyChanges(t, applier, batchFor(epoch,
		accountChange(t, 12, account, projection.LifecycleSuspended),
	))
	suspended := mustLookup(t, creds, key)
	if suspended.AccountState == nil || *suspended.AccountState != string(projection.LifecycleSuspended) {
		t.Errorf("account state = %v, want suspended", suspended.AccountState)
	}
	if suspended.KeyState != string(projection.CredentialRevoked) || suspended.RevokedAt == nil {
		t.Errorf("key = %s/%v, want the revocation untouched by the account's move", suspended.KeyState, suspended.RevokedAt)
	}
}

func TestIntegrationCredentialLookupReportsAClosedAccount(t *testing.T) {
	// The terminal account state arrives as data, not as a miss: the
	// credential row is still there and its absence-delete has not run, so
	// the read returns closed and the caller refuses — the mirror never
	// pre-digests a lifecycle into a verdict.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	_, applier, creds := integrationCredentials(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 4,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleClosed}},
	})

	view := mustLookup(t, creds, key)
	if view.AccountState == nil || *view.AccountState != string(projection.LifecycleClosed) {
		t.Errorf("account state = %v, want closed carried as data for the caller to refuse", view.AccountState)
	}
}

func TestIntegrationCredentialLookupMissIsASentinel(t *testing.T) {
	_, applier, creds := integrationCredentials(t)

	view, err := creds.Lookup(t.Context(), projectionUUID(t))
	if !errors.Is(err, persistence.ErrCredentialNotFound) {
		t.Fatalf("err = %v, want it to wrap ErrCredentialNotFound", err)
	}
	if view != (persistence.CredentialView{}) {
		t.Errorf("view = %+v on a miss, want the zero view — the caller synthesizes its own", view)
	}

	// The other path to a miss: a snapshot's absence delete, the way the
	// mirror stops serving a key the Control Plane no longer projects.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 1,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})
	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 2,
	})
	if _, err := creds.Lookup(t.Context(), key); !errors.Is(err, persistence.ErrCredentialNotFound) {
		t.Errorf("err after the absence delete = %v, want ErrCredentialNotFound", err)
	}
}

func TestIntegrationCredentialLookupReadsInsideTheCallersUnitOfWork(t *testing.T) {
	// Like every query in this port, the lookup resolves its handle from the
	// context: inside a unit of work it joins that transaction. The
	// admission path runs it standalone, but the join is the port's own
	// contract, and this pins it against the store's WithinTx machinery.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier, creds := integrationCredentials(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 2,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})

	store := New(db)
	err := store.WithinTx(t.Context(), func(txCtx context.Context) error {
		view, lookupErr := creds.Lookup(txCtx, key)
		if lookupErr != nil {
			return lookupErr
		}
		if view.Digest != projectionDigestB {
			t.Errorf("digest inside the unit of work = %q, want the mirrored row", view.Digest)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Lookup inside a unit of work: %v", err)
	}
}
