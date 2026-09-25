package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
)

// stubProjections is the applier the application tests drive the use cases
// with: it records every handover — including the instant a snapshot apply
// stamps, which the tests must see to believe the port received one — and
// answers with one configured position, one configured acknowledgement and
// one configured error.
type stubProjections struct {
	position    projection.Position
	positionErr error
	ack         uint64
	err         error

	snapshots []projection.Snapshot
	appliedAt []time.Time
	batches   []projection.Batch
	calls     int
}

// Position implements persistence.ProjectionApplier.
func (stub *stubProjections) Position(_ context.Context) (projection.Position, error) {
	return stub.position, stub.positionErr
}

// ApplySnapshot implements persistence.ProjectionApplier.
func (stub *stubProjections) ApplySnapshot(_ context.Context, snapshot projection.Snapshot, appliedAt time.Time) (uint64, error) {
	stub.calls++
	stub.snapshots = append(stub.snapshots, snapshot)
	stub.appliedAt = append(stub.appliedAt, appliedAt)
	return stub.ack, stub.err
}

// ApplyChanges implements persistence.ProjectionApplier.
func (stub *stubProjections) ApplyChanges(_ context.Context, batch projection.Batch) (uint64, error) {
	stub.calls++
	stub.batches = append(stub.batches, batch)
	return stub.ack, stub.err
}

// The grammar fixtures below are the domain package's rules exercised through
// the use case, which is the boundary the listener actually calls. What is
// under test here is the ordering: grammar first, store second — a message
// that would violate the mirror's own constraints must be refused before a
// transaction opens.

const (
	epoch     = "0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90"
	keyID     = "11111111-2222-4333-8444-555555555555"
	accountID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

func TestApplyProjectionSnapshotJudgesTheGrammarBeforeTheStore(t *testing.T) {
	snapshot := projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion + 1,
		Epoch:            epoch,
		SnapshotRevision: 3,
	}

	applier := &stubProjections{}
	app := New("v0.1.0", &stubFacts{}, newCatalog(newCatalogWorld()), applier)
	if _, err := app.ApplyProjectionSnapshot(context.Background(), snapshot); !errors.Is(err, projection.ErrUnsupportedVersion) {
		t.Fatalf("err = %v, want it to wrap ErrUnsupportedVersion", err)
	}
	if applier.calls != 0 {
		t.Errorf("the store was touched %d times; a message outside the grammar must be refused before it", applier.calls)
	}
}

func TestApplyProjectionSnapshotHandsTheSnapshotToThePort(t *testing.T) {
	snapshot := projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 3,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: keyID, AccountID: accountID, Digest: strings.Repeat("a", 64),
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: accountID, State: projection.LifecycleActive}},
	}

	applier := &stubProjections{ack: 3}
	app := New("v0.1.0", &stubFacts{}, newCatalog(newCatalogWorld()), applier)
	applied, err := app.ApplyProjectionSnapshot(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("ApplyProjectionSnapshot() error = %v", err)
	}
	if applied != 3 {
		t.Errorf("applied = %d, want the port's acknowledgement", applied)
	}
	if len(applier.snapshots) != 1 || applier.snapshots[0].SnapshotRevision != 3 || len(applier.snapshots[0].APIKeys) != 1 {
		t.Fatalf("the port received %+v, want the snapshot whole", applier.snapshots)
	}
	if applier.appliedAt[0].IsZero() {
		t.Error("the snapshot apply was handed a zero instant; the mirror rows would carry no application time")
	}
}

func TestApplyProjectionChangesJudgesTheGrammarBeforeTheStore(t *testing.T) {
	tests := []struct {
		name  string
		batch projection.Batch
		want  error
	}{
		{
			name: "a version this plane does not speak",
			batch: projection.Batch{
				ProtocolVersion: projection.ProtocolVersion + 1,
				Epoch:           epoch,
				Changes:         []projection.Change{{Revision: 1, Kind: projection.KindAccount, Account: &projection.AccountState{AccountID: accountID, State: projection.LifecycleActive}}},
			},
			want: projection.ErrUnsupportedVersion,
		},
		{
			name: "an empty batch",
			batch: projection.Batch{
				ProtocolVersion: projection.ProtocolVersion,
				Epoch:           epoch,
			},
			want: projection.ErrBatchShape,
		},
		{
			name: "revisions that are not contiguous",
			batch: projection.Batch{
				ProtocolVersion: projection.ProtocolVersion,
				Epoch:           epoch,
				Changes: []projection.Change{
					{Revision: 4, Kind: projection.KindAccount, Account: &projection.AccountState{AccountID: accountID, State: projection.LifecycleActive}},
					{Revision: 6, Kind: projection.KindAccount, Account: &projection.AccountState{AccountID: accountID, State: projection.LifecycleActive}},
				},
			},
			want: projection.ErrBatchShape,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applier := &stubProjections{}
			app := New("v0.1.0", &stubFacts{}, newCatalog(newCatalogWorld()), applier)
			if _, err := app.ApplyProjectionChanges(context.Background(), tt.batch); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.want)
			}
			if applier.calls != 0 {
				t.Errorf("the store was touched %d times; a batch outside the grammar must be refused before it", applier.calls)
			}
		})
	}
}

// TestApplyProjectionChangesParsesPayloadsAtTheBoundary covers the one thing
// the listener's decode cannot decide for itself: a change entry's payload is
// grammar-checked when the batch is built, so a payload missing its digest is
// refused before the position is ever read — and a valid payload arrives at
// the port as a parsed record, not as bytes to parse twice.
func TestApplyProjectionChangesParsesPayloadsAtTheBoundary(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"account_id": accountID,
		"digest":     strings.Repeat("b", 64),
		"state":      "active",
		"revoked_at": nil,
	})
	if err != nil {
		t.Fatalf("building a payload: %v", err)
	}

	applier := &stubProjections{ack: 1}
	app := New("v0.1.0", &stubFacts{}, newCatalog(newCatalogWorld()), applier)
	change, err := projection.NewChange(1, projection.KindAPIKey, keyID, time.Now().UTC(), payload)
	if err != nil {
		t.Fatalf("building a change: %v", err)
	}
	applied, err := app.ApplyProjectionChanges(context.Background(), projection.Batch{
		ProtocolVersion: projection.ProtocolVersion,
		Epoch:           epoch,
		FromRevision:    0,
		Changes:         []projection.Change{change},
	})
	if err != nil {
		t.Fatalf("ApplyProjectionChanges() error = %v", err)
	}
	if applied != 1 {
		t.Errorf("applied = %d, want the port's acknowledgement", applied)
	}
	if len(applier.batches) != 1 || applier.batches[0].Changes[0].Credential == nil {
		t.Fatalf("the port received %+v, want the change parsed into a record", applier.batches)
	}
	if got := applier.batches[0].Changes[0].Credential.Digest; got != strings.Repeat("b", 64) {
		t.Errorf("the parsed credential's digest = %q, want the payload's", got)
	}
}

func TestProjectionPositionPassesThePortsAnswerThrough(t *testing.T) {
	// The position is this plane's own fact and the use case has no opinion
	// about it — it is read once per producer cycle and decided on over
	// there. Rewriting it here would be a second definition of "where the
	// mirror stands".
	position := projection.Position{Epoch: epoch, Bootstrapped: true, AppliedRevision: 9}

	applier := &stubProjections{position: position}
	app := New("v0.1.0", &stubFacts{}, newCatalog(newCatalogWorld()), applier)
	got, err := app.ProjectionPosition(context.Background())
	if err != nil {
		t.Fatalf("ProjectionPosition() error = %v", err)
	}
	if got != position {
		t.Errorf("ProjectionPosition() = %+v, want the port's answer unchanged", got)
	}

	applier.positionErr = errors.New("the store is unreachable")
	if _, err := app.ProjectionPosition(context.Background()); err == nil {
		t.Error("ProjectionPosition() reported no error for an unreadable store")
	}
}
