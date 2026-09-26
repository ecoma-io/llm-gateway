package application

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
)

// The admission tests. Every one of them runs the real use case over the one
// fake world, and every assertion is about the relationship the flow exists
// to keep: what was written, in what order, in whose unit of work, and what
// the caller was told. No assertion renders a credential, a digest or a body
// — a test failure names the decision that went wrong, never the material
// that would let one be replayed.

const (
	// admissionKeyID is a valid version-4 UUID spelling, because the token
	// grammar's key-id check runs before any lookup and a malformed id would
	// test the parse, not the admission.
	admissionKeyID   = "3f2b8a4e-1234-4abc-8def-0123456789ab"
	admissionAccount = "account-1"
)

// admissionSecret is exactly the 32 bytes the token grammar demands. The
// digest the tests compare against is always computed through
// execution.SecretDigest — never spelled as a literal — so the test cannot
// drift from the domain's definition of a digest.
var admissionSecret = []byte("0123456789abcdef0123456789abcdef")

func admissionToken(secret []byte) string {
	// The token grammar's exact spelling: brand, key id, and the raw-url
	// base64 of the 32 secret bytes — the same encoder the domain decodes
	// with, so a test token is canonical by construction.
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	return "gw_" + admissionKeyID + "_" + encoded
}

func admissionBody(model string) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"content":"hello"}]}`, model))
}

// admissionInput is the transport-shaped input for the model the fixture
// seeds. The use case never sees a credential — verification happened before
// Serve — so the account facts ride beside the key identity, exactly as the
// handler copies them.
func admissionInput(body []byte) ChatInput {
	return ChatInput{
		RequestID:      identity.RequestID("req-transport-1"),
		Credential:     admissionKeyID,
		AccountID:      admissionAccount,
		AccountState:   statePtr(string(projection.LifecycleActive)),
		IdempotencyKey: "replay-key-1",
		Body:           BodyBytes(body),
	}
}

func statePtr(state string) *string { return &state }

// admissionFixture builds the smallest world that admits: one verified key
// with its active account, one alias, one price snapshot, one eligible grant.
// The prices are minor units per 1M tokens, so the hold a default body draws
// is ceil((71·1_000_000 + 16·2_000_000) / 1_000_000) = 103 minor units — 71
// being the whole body's byte length, the canonical v1 input count, not the
// content's 5.
func admissionFixture(t *testing.T) (*ChatAdmission, *admissionWorld) {
	t.Helper()
	world := newAdmissionWorld()
	world.seedCredential(admissionKeyID, admissionAccount,
		execution.SecretDigest(admissionSecret), string(projection.CredentialActive),
		statePtr(string(projection.LifecycleActive)))
	alias := world.seedAlias("test-model", 4096, 1000)
	world.seedPrice(alias.ID, catalog.PriceSnapshot{RevisionID: "rev-1", InputUnitPrice: 1_000_000, OutputUnitPrice: 2_000_000})
	world.seedBucket(admissionAccount, "bucket-1", 10_000, true)
	return newAdmission(world), world
}

// wantEvents fails the test with the first divergent step named, which is how
// an ordering assertion stays readable when the sequence is a dozen entries
// long.
func wantEvents(t *testing.T, world *admissionWorld, want []string) {
	t.Helper()
	for i, step := range want {
		if i >= len(world.events) {
			t.Fatalf("the world ran out of steps at %d: got %v, want %v", i, world.events, want)
		}
		if world.events[i] != step {
			t.Fatalf("step %d = %q, want %q (full sequence %v)", i, world.events[i], step, world.events)
		}
	}
	if len(world.events) != len(want) {
		t.Fatalf("the world recorded %d steps, want %d (got %v)", len(world.events), len(want), world.events)
	}
}

func admissionRequestRow(t *testing.T, world *admissionWorld) execution.Request {
	t.Helper()
	if len(world.requests) != 1 {
		t.Fatalf("the world holds %d request rows, want 1", len(world.requests))
	}
	for _, row := range world.requests {
		return row
	}
	return execution.Request{}
}

func admissionReservationRow(t *testing.T, world *admissionWorld) accounting.Reservation {
	t.Helper()
	if len(world.reservations) != 1 {
		t.Fatalf("the world holds %d reservations, want 1", len(world.reservations))
	}
	for _, row := range world.reservations {
		return row
	}
	return accounting.Reservation{}
}

func admissionIntakeRow(t *testing.T, world *admissionWorld, account, key string) execution.Intake {
	t.Helper()
	intake, ok := world.intakes[world.intakeKey(account, key)]
	if !ok {
		t.Fatalf("the world holds no replay record for the request's account and key")
	}
	return intake
}

// fakePGError is a store failure with an SQLSTATE, the shape the retry
// classifier asks for structurally — the same question pgconn's error type
// answers on the production path.
type fakePGError struct{ code string }

func (e fakePGError) Error() string    { return "fake: store failure with sqlstate " + e.code }
func (e fakePGError) SQLState() string { return e.code }

// ---------------------------------------------------------------------------
// credential verification
// ---------------------------------------------------------------------------

// TestAuthenticateVerifiesOrRefuses walks the verification matrix. The
// constant the table pins is that every refusal is the same typed answer —
// the reasons differ for the log, never for the caller — and that a refusal
// writes nothing at all: verification precedes admission, and what it refuses
// never became a request.
func TestAuthenticateVerifiesOrRefuses(t *testing.T) {
	fixture := func(mutate func(world *admissionWorld)) (*CredentialAuthenticator, *admissionWorld) {
		_, world := admissionFixture(t)
		authn := newCredentialAuthenticator(world)
		if mutate != nil {
			mutate(world)
		}
		return authn, world
	}

	for _, tt := range []struct {
		name        string
		credential  string
		mutate      func(world *admissionWorld)
		wantReason  UnauthenticatedReason
		wantKeyID   string
		wantAccount string
		wantFailure bool
	}{
		{
			name:        "a well-formed token over a mirrored secret verifies",
			credential:  admissionToken(admissionSecret),
			wantKeyID:   admissionKeyID,
			wantAccount: admissionAccount,
		},
		{
			name:       "a token that is not a token is refused before the mirror is read",
			credential: "not-a-token",
			wantReason: ReasonCredentialUnknown,
		},
		{
			name:       "a token over a key the mirror does not carry is refused",
			credential: admissionToken([]byte("99999999999999999999999999999999")),
			wantReason: ReasonCredentialUnknown,
		},
		{
			name:       "a token over a mirrored key whose secret does not match is refused",
			credential: admissionToken([]byte("99999999999999999999999999999999")),
			mutate: func(world *admissionWorld) {
				world.seedCredential(admissionKeyID, admissionAccount,
					execution.SecretDigest(admissionSecret), string(projection.CredentialActive),
					statePtr(string(projection.LifecycleActive)))
			},
			wantReason: ReasonCredentialUnknown,
		},
		{
			name:       "a mirrored key whose state is not admitting is refused",
			credential: admissionToken(admissionSecret),
			mutate: func(world *admissionWorld) {
				world.seedCredential(admissionKeyID, admissionAccount,
					execution.SecretDigest(admissionSecret), string(projection.CredentialRevoked),
					statePtr(string(projection.LifecycleActive)))
			},
			wantReason: ReasonCredentialRevoked,
		},
		{
			name:       "a mirrored key whose account row is absent is refused closed",
			credential: admissionToken(admissionSecret),
			mutate: func(world *admissionWorld) {
				world.seedCredential(admissionKeyID, admissionAccount,
					execution.SecretDigest(admissionSecret), string(projection.CredentialActive),
					nil)
			},
			wantReason: ReasonAccountAbsent,
		},
		{
			name:       "a mirror that cannot answer is a failure, not a verdict",
			credential: admissionToken(admissionSecret),
			mutate: func(world *admissionWorld) {
				world.credentialFailure = errors.New("fake: the mirror is unreachable")
			},
			wantFailure: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			authn, world := fixture(tt.mutate)
			verified, err := authn.Authenticate(context.Background(), tt.credential)

			if tt.wantFailure {
				if err == nil {
					t.Fatalf("Authenticate succeeded, want the mirror's failure to surface")
				}
				var refusal *Unauthenticated
				if errors.As(err, &refusal) {
					t.Fatalf("the mirror's failure arrived as a verification verdict %v, want a plain error", refusal.Reason)
				}
				if len(world.events) != 0 {
					t.Fatalf("the failure wrote %v, want nothing", world.events)
				}
				return
			}

			if tt.wantReason == "" && err != nil {
				t.Fatalf("Authenticate refused a credential that should verify: %v", err)
			}
			if tt.wantReason != "" {
				var refusal *Unauthenticated
				if !errors.As(err, &refusal) {
					t.Fatalf("Authenticate err = %v, want an Unauthenticated refusal", err)
				}
				if refusal.Reason != tt.wantReason {
					t.Fatalf("refusal reason = %q, want %q", refusal.Reason, tt.wantReason)
				}
			}
			if tt.wantKeyID != "" {
				if verified.KeyID != tt.wantKeyID {
					t.Fatalf("verified key id = %q, want the presented token's own key id", verified.KeyID)
				}
				if verified.AccountID != tt.wantAccount {
					t.Fatalf("verified account id = %q, want the mirror's owner", verified.AccountID)
				}
				if verified.AccountState == nil || *verified.AccountState != string(projection.LifecycleActive) {
					t.Fatalf("verified account state = %v, want the mirror's active lifecycle", verified.AccountState)
				}
			}
			if len(world.events) != 0 {
				t.Fatalf("verification wrote %v, want nothing — a refused credential never became a request", world.events)
			}
		})
	}
}

// TestAuthenticateParsesBeforeItLooks pins the order the leak-resistance
// depends on: a token that fails the grammar never reaches the mirror, so the
// number of lookups a malformed token costs is zero.
func TestAuthenticateParsesBeforeItLooks(t *testing.T) {
	_, world := admissionFixture(t)
	authn := newCredentialAuthenticator(world)
	if _, err := authn.Authenticate(context.Background(), "garbage"); err == nil {
		t.Fatalf("Authenticate accepted a token that is not a token")
	}
	if world.credentialLookups != 0 {
		t.Fatalf("the mirror was read %d times for an unparseable token, want 0", world.credentialLookups)
	}
}

// TestAuthenticateMissAndMismatchAreTheSameAnswer pins the shape the
// constant-time burn exists to keep: an unknown key id and a known key with
// the wrong secret are refused identically. The digest comparison itself runs
// either way — the miss synthesizes the zero credential so it computes and
// compares the presented secret's digest exactly as a match would — but a
// test cannot observe a constant, only its consequence, and the consequence
// is that these two answers are indistinguishable.
func TestAuthenticateMissAndMismatchAreTheSameAnswer(t *testing.T) {
	_, world := admissionFixture(t)
	authn := newCredentialAuthenticator(world)

	_, missErr := authn.Authenticate(context.Background(), admissionToken([]byte("88888888888888888888888888888888")))
	if missErr == nil {
		t.Fatalf("the unknown key verified, want a refusal")
	}
	_, mismatchErr := authn.Authenticate(context.Background(), admissionToken([]byte("77777777777777777777777777777777")))
	if mismatchErr == nil {
		t.Fatalf("the wrong secret verified, want a refusal")
	}
	var miss, mismatch *Unauthenticated
	if !errors.As(missErr, &miss) || !errors.As(mismatchErr, &mismatch) {
		t.Fatalf("the refusals lost their type: %v / %v", missErr, mismatchErr)
	}
	if miss.Reason != mismatch.Reason {
		t.Fatalf("the miss answered %q and the mismatch %q, want one answer", miss.Reason, mismatch.Reason)
	}
}

// ---------------------------------------------------------------------------
// the gates before any unit of work that needs a body
// ---------------------------------------------------------------------------

// TestAdmissionAccountGateRefusesWithARowAndNothingElse: a suspended or closed
// account is refused with the rejected request row alone — no body was read,
// so there is no digest, and no replay record could be keyed.
func TestAdmissionAccountGateRefusesWithARowAndNothingElse(t *testing.T) {
	for _, tt := range []struct {
		name       string
		state      string
		wantReason execution.RejectionReason
	}{
		{name: "a suspended account", state: string(projection.LifecycleSuspended), wantReason: execution.RejectedAccountSuspended},
		{name: "a closed account", state: string(projection.LifecycleClosed), wantReason: execution.RejectedAccountClosed},
		{
			name:       "a lifecycle the projection does not declare",
			state:      "archived",
			wantReason: execution.RejectedAccountSuspended,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCase, world := admissionFixture(t)
			in := admissionInput(admissionBody("test-model"))
			in.AccountState = statePtr(tt.state)

			outcome, err := useCase.Serve(context.Background(), in)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != OutcomeRejected || outcome.Reason != tt.wantReason {
				t.Fatalf("outcome = %s/%s, want rejected/%s", outcome.Kind, outcome.Reason, tt.wantReason)
			}
			if outcome.Detail != DetailNone {
				t.Fatalf("detail = %q, want none — the account is not a field of the request", outcome.Detail)
			}
			wantEvents(t, world, []string{"begin", "request.insert", "commit"})
			row := admissionRequestRow(t, world)
			if row.Status != execution.StatusRejected || row.RejectionReason != tt.wantReason {
				t.Fatalf("the row is %s/%s, want rejected/%s", row.Status, row.RejectionReason, tt.wantReason)
			}
			if row.APIKeyID != admissionKeyID || row.AccountID != admissionAccount {
				t.Fatalf("the row does not name the credential and account that arrived")
			}
			if row.Alias != "" {
				t.Fatalf("the row names an alias, want the empty one — no alias was read")
			}
			if len(world.intakes) != 0 || len(world.reservations) != 0 {
				t.Fatalf("the gate wrote a replay record or a hold, want the row alone")
			}
		})
	}
}

// TestAdmissionRefusesAKeyOutsideTheGrammar: the key's own field is named,
// and the refusal is the row alone, because a key that cannot be stored
// cannot key a replay record.
func TestAdmissionRefusesAKeyOutsideTheGrammar(t *testing.T) {
	for _, tt := range []struct {
		name string
		key  string
	}{
		{name: "no key at all", key: ""},
		{name: "a key with a leading space", key: " replay-key-1"},
		{name: "a key past the length bound", key: strings.Repeat("k", 257)},
		{name: "a key outside visible ascii", key: "ключ-1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCase, world := admissionFixture(t)
			in := admissionInput(admissionBody("test-model"))
			in.IdempotencyKey = tt.key

			outcome, err := useCase.Serve(context.Background(), in)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != OutcomeRejected || outcome.Reason != execution.RejectedInvalidRequest {
				t.Fatalf("outcome = %s/%s, want rejected/invalid_request", outcome.Kind, outcome.Reason)
			}
			if outcome.Detail != DetailIdempotencyKey {
				t.Fatalf("detail = %q, want the key's own field", outcome.Detail)
			}
			wantEvents(t, world, []string{"begin", "request.insert", "commit"})
			if len(world.intakes) != 0 {
				t.Fatalf("the grammar refusal keyed a replay record, want the row alone")
			}
		})
	}
}

// TestAdmissionRefusesABodyThatNeverArrived: both refusal reasons answer the
// same way — the row, no detail, no replay record, because there is nothing
// to digest a replay against.
func TestAdmissionRefusesABodyThatNeverArrived(t *testing.T) {
	for _, tt := range []struct {
		name    string
		refusal BodyRefusal
	}{
		{name: "a body over the transport's bound", refusal: BodyTooLarge},
		{name: "a body that broke mid-read", refusal: BodyUnreadable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCase, world := admissionFixture(t)
			in := admissionInput(nil)
			in.Body = BodyRefused{Reason: tt.refusal}

			outcome, err := useCase.Serve(context.Background(), in)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != OutcomeRejected || outcome.Reason != execution.RejectedInvalidRequest {
				t.Fatalf("outcome = %s/%s, want rejected/invalid_request", outcome.Kind, outcome.Reason)
			}
			if outcome.Detail != DetailNone {
				t.Fatalf("detail = %q, want none — the body is not one field of the request", outcome.Detail)
			}
			wantEvents(t, world, []string{"begin", "request.insert", "commit"})
			if len(world.intakes) != 0 {
				t.Fatalf("a body that never arrived keyed a replay record, want the row alone")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the replay record, asked before any unit of work opens
// ---------------------------------------------------------------------------

// seedReplayRecord plants an intake the way a previous request left it.
func seedReplayRecord(world *admissionWorld, digest string, final *execution.FinalStatus, reason execution.RejectionReason, requestID identity.RequestID) {
	world.intakes[world.intakeKey(admissionAccount, "replay-key-1")] = execution.Intake{
		AccountID:            admissionAccount,
		IdempotencyKey:       "replay-key-1",
		RequestDigest:        digest,
		RequestID:            requestID,
		FinalStatus:          final,
		FinalRejectionReason: reason,
	}
}

func finalStatusPtr(status execution.FinalStatus) *execution.FinalStatus { return &status }

// TestAdmissionAnswersFromTheRecordBeforeAnyUnitOpens: a key that already owns
// a decision is answered from the record, and no unit of work opens at all —
// the probe is the cheapest answer admission has.
func TestAdmissionAnswersFromTheRecordBeforeAnyUnitOpens(t *testing.T) {
	body := admissionBody("test-model")
	digest := execution.SecretDigest(body)

	for _, tt := range []struct {
		name       string
		seed       func(world *admissionWorld)
		wantKind   OutcomeKind
		wantReason execution.RejectionReason
		wantOrig   identity.RequestID
		wantFail   bool
	}{
		{
			name:     "the same key and bytes still deciding answers in flight",
			seed:     func(world *admissionWorld) { seedReplayRecord(world, digest, nil, "", "original-1") },
			wantKind: OutcomeInFlight,
		},
		{
			name: "the same key and bytes already refused answers the refusal again",
			seed: func(world *admissionWorld) {
				seedReplayRecord(world, digest, finalStatusPtr(execution.FinalRejected), execution.RejectedUnknownAlias, "original-1")
			},
			wantKind:   OutcomeReplay,
			wantReason: execution.RejectedUnknownAlias,
			wantOrig:   "original-1",
		},
		{
			name: "the same key over different bytes answers a conflict",
			seed: func(world *admissionWorld) {
				seedReplayRecord(world, execution.SecretDigest([]byte(`{"other":true}`)), nil, "", "original-1")
			},
			wantKind: OutcomeConflict,
		},
		{
			name: "the same key and bytes whose original succeeded is a failure, not an answer",
			seed: func(world *admissionWorld) {
				seedReplayRecord(world, digest, finalStatusPtr(execution.FinalSucceeded), "", "original-1")
			},
			wantFail: true,
		},
		{
			name: "the same key and bytes whose original failed mid-stream is a failure, not an answer",
			seed: func(world *admissionWorld) {
				world.intakes[world.intakeKey(admissionAccount, "replay-key-1")] = execution.Intake{
					AccountID:          admissionAccount,
					IdempotencyKey:     "replay-key-1",
					RequestDigest:      digest,
					RequestID:          "original-1",
					FinalStatus:        finalStatusPtr(execution.FinalFailed),
					FinalFailureReason: execution.FailedStreamAfterCommitment,
				}
			},
			wantFail: true,
		},
		{
			name: "the same key and bytes whose original the runtime abandoned answers the spent key",
			seed: func(world *admissionWorld) {
				world.intakes[world.intakeKey(admissionAccount, "replay-key-1")] = execution.Intake{
					AccountID:          admissionAccount,
					IdempotencyKey:     "replay-key-1",
					RequestDigest:      digest,
					RequestID:          "original-1",
					FinalStatus:        finalStatusPtr(execution.FinalFailed),
					FinalFailureReason: execution.FailedGatewayAbandoned,
				}
			},
			wantKind: OutcomeUnanswered,
			wantOrig: "original-1",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCase, world := admissionFixture(t)
			tt.seed(world)
			in := admissionInput(body)

			outcome, err := useCase.Serve(context.Background(), in)
			if tt.wantFail {
				if err == nil {
					t.Fatalf("Serve answered a record this build cannot answer, want the failure")
				}
				if len(world.events) != 0 {
					t.Fatalf("the failure wrote %v, want nothing", world.events)
				}
				return
			}
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != tt.wantKind {
				t.Fatalf("outcome kind = %s, want %s", outcome.Kind, tt.wantKind)
			}
			if outcome.Reason != tt.wantReason {
				t.Fatalf("outcome reason = %q, want %q", outcome.Reason, tt.wantReason)
			}
			if outcome.Original != tt.wantOrig {
				t.Fatalf("outcome original = %q, want %q", outcome.Original, tt.wantOrig)
			}
			// Every decision names the arrival that reached it: the probe's
			// answers carry the transport-minted id, since no unit opened to
			// mint another.
			if outcome.RuntimeRequestID != in.RequestID {
				t.Fatalf("outcome runtime request id = %q, want the arrival's %q", outcome.RuntimeRequestID, in.RequestID)
			}
			if len(world.events) != 0 {
				t.Fatalf("the probe wrote %v, want nothing — the record already owned the answer", world.events)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the admission unit, and the seam behind it
// ---------------------------------------------------------------------------

// TestAdmissionHandsAnAdmittedRequestToTheRoutingStage walks the happy
// admission path: the unit opens once, writes the order request-lifecycle.md
// pins — drawdown, request row, hold, replay record last — commits, and hands
// the caller an admission payload for the stage that routes it. What happens
// after the hand-off is route_test.go's subject; this test pins that
// admission alone writes no ending and states no answer.
func TestAdmissionHandsAnAdmittedRequestToTheRoutingStage(t *testing.T) {
	useCase, world := admissionFixture(t)
	body := admissionBody("test-model")

	outcome, err := useCase.Serve(context.Background(), admissionInput(body))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeAdmitted || outcome.Admitted == nil {
		t.Fatalf("outcome = %s, want admitted with a payload", outcome.Kind)
	}
	if outcome.Admitted.Alias != "test-model" || outcome.Admitted.InputTokens != 71 {
		t.Fatalf("the admission payload lost its alias or its input count")
	}

	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
	})

	// The hold: drawn at 103, still open — no ending has been stated, and
	// stating one is the routing stage's job now.
	reservation := admissionReservationRow(t, world)
	if reservation.ReservedAmount != 103 {
		t.Fatalf("the hold = %d, want 103", reservation.ReservedAmount)
	}
	if reservation.State != accounting.StateOpen {
		t.Fatalf("the hold is %s, want open — admission writes no ending", reservation.State)
	}
	if !reservation.ExpiresAt.Equal(world.now.Add(time.Minute)) {
		t.Fatalf("expires_at is not the hold window past the unit's clock")
	}
	if !reservation.LeaseExpiresAt.Equal(world.now.Add(30 * time.Second)) {
		t.Fatalf("lease_expires_at is not the lease TTL past the unit's clock")
	}
	if reservation.LeaseOwner != "test-host:1" {
		t.Fatalf("lease owner = %q, want the configured owner", reservation.LeaseOwner)
	}
	if len(reservation.Allocations) != 1 || reservation.Allocations[0].FundingBucketID != "bucket-1" ||
		reservation.Allocations[0].Amount != 103 || reservation.Allocations[0].Ordinal != 1 {
		t.Fatalf("the hold's split does not name bucket-1 for all 103 at ordinal 1")
	}

	// The request row: born executing, carrying the price basis and the
	// bounds the eventual fact will be priced and audited from.
	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusExecuting {
		t.Fatalf("the request is %s, want executing", row.Status)
	}
	if row.Alias != "test-model" || row.InputTokens != 71 || row.MaxOutputTokens != 16 {
		t.Fatalf("the request row's bounds drifted from the body and the alias")
	}
	if row.Price.RevisionID != "rev-1" {
		t.Fatalf("the request row lost its pricing basis")
	}

	// The replay record: digested over the raw bytes, watching — terminal
	// only when the ending's unit takes its pointer.
	intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1")
	if intake.RequestDigest != execution.SecretDigest(body) {
		t.Fatalf("the replay record's digest is not the digest of the raw bytes")
	}
	if intake.FinalStatus != nil {
		t.Fatalf("the replay record is terminal, want it still waiting on the ending")
	}

	// The payload carries what the routing stage needs and re-reads nothing
	// for: the frozen price, the live reservation, the legs as granted.
	if outcome.Admitted.ReservationID != reservation.ID || len(outcome.Admitted.Legs) != 1 {
		t.Fatalf("the admission payload does not name the hold it opened")
	}
	if outcome.Admitted.Price.RevisionID != "rev-1" || outcome.Admitted.Hold != 103 {
		t.Fatalf("the admission payload's pricing drifted from the unit's decision")
	}
}

// TestAdmissionAdmitsContentThatCarriesNoBytes pins the refusal boundary's
// move to the message list: the emptiness that refuses a body is a
// conversation with no messages, not content with no bytes. An empty content
// string is a message the API contract allows, the body it travels in still
// prices a whole-body hold, and no rejection pair is written for it.
func TestAdmissionAdmitsContentThatCarriesNoBytes(t *testing.T) {
	useCase, _ := admissionFixture(t)
	body := []byte(`{"model":"test-model","max_tokens":16,"messages":[{"content":""}]}`)

	outcome, err := useCase.Serve(context.Background(), admissionInput(body))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeAdmitted || outcome.Admitted == nil {
		t.Fatalf("outcome = %s, want admitted — an empty message list refuses a body, empty content does not", outcome.Kind)
	}
	if outcome.Admitted.InputTokens != len(body) {
		t.Fatalf("input tokens = %d, want the body's %d bytes — the hold prices the whole body", outcome.Admitted.InputTokens, len(body))
	}
}

// TestAdmissionRefusalsInsideTheUnitWriteThePairAndCommit walks every refusal
// the admission unit itself decides. The constant across the table: the
// decision is written as the rejection pair — the rejected request row and
// the replay record born terminal — the unit COMMITS, and the caller gets the
// outcome with a nil error.
func TestAdmissionRefusalsInsideTheUnitWriteThePairAndCommit(t *testing.T) {
	hugePrice := func(world *admissionWorld) {
		world.seedPrice("alias-test-model", catalog.PriceSnapshot{
			RevisionID: "rev-big", InputUnitPrice: math.MaxInt64, OutputUnitPrice: math.MaxInt64,
		})
	}
	for _, tt := range []struct {
		name       string
		body       []byte
		setup      func(world *admissionWorld)
		wantReason execution.RejectionReason
		wantDetail RejectionDetail
	}{
		{
			name:       "a name no catalog row carries",
			body:       admissionBody("no-such-model"),
			wantReason: execution.RejectedUnknownAlias,
			wantDetail: DetailNone,
		},
		{
			name:       "a body that is not json",
			body:       []byte("{not json"),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailNone,
		},
		{
			name:       "a body with no model",
			body:       []byte(`{"max_tokens":16,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailModel,
		},
		{
			name:       "a model that is not a string",
			body:       []byte(`{"model":7,"max_tokens":16,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailModel,
		},
		{
			name:       "an empty model",
			body:       []byte(`{"model":"","max_tokens":16,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailModel,
		},
		{
			name:       "a model past the grammar's length",
			body:       []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"content":"hello"}]}`, strings.Repeat("a", 200))),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailModel,
		},
		{
			name:       "a model past the provider's own bound",
			body:       []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"content":"hello"}]}`, strings.Repeat("a", 300))),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailModel,
		},
		{
			name:       "a model with a character the grammar does not carry",
			body:       []byte(`{"model":"open ai","max_tokens":16,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailModel,
		},
		{
			name:       "a multibyte model",
			body:       []byte(`{"model":"mødel","max_tokens":16,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailModel,
		},
		{
			name:       "no output ceiling spelled at all",
			body:       []byte(`{"model":"test-model","messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxTokens,
		},
		{
			name:       "a null output ceiling",
			body:       []byte(`{"model":"test-model","max_tokens":null,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxTokens,
		},
		{
			name:       "a zero ceiling",
			body:       []byte(`{"model":"test-model","max_tokens":0,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxTokens,
		},
		{
			name:       "a ceiling spelled as text",
			body:       []byte(`{"model":"test-model","max_tokens":"16","messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxTokens,
		},
		{
			name:       "a fractional ceiling",
			body:       []byte(`{"model":"test-model","max_tokens":1.5,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxTokens,
		},
		{
			name:       "a ceiling past the integer columns' bound",
			body:       []byte(`{"model":"test-model","max_tokens":2147483648,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxTokens,
		},
		{
			name:       "a zero ceiling under the other spelling",
			body:       []byte(`{"model":"test-model","max_completion_tokens":0,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxCompletionTokens,
		},
		{
			name:       "two ceilings that disagree",
			body:       []byte(`{"model":"test-model","max_tokens":16,"max_completion_tokens":17,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxTokens,
		},
		{
			name:       "a ceiling past the alias's own bound",
			body:       []byte(`{"model":"test-model","max_tokens":5000,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxTokens,
		},
		{
			name:       "a ceiling past the other spelling's own bound",
			body:       []byte(`{"model":"test-model","max_completion_tokens":5000,"messages":[{"content":"hello"}]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailMaxCompletionTokens,
		},
		{
			name:       "no content anywhere in the request",
			body:       []byte(`{"model":"test-model","max_tokens":16,"messages":[]}`),
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailNone,
		},
		{
			name:       "a hold the arithmetic refuses",
			body:       admissionBody("test-model"),
			setup:      hugePrice,
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailNone,
		},
		{
			name:       "a hold past the alias's reservation cap",
			body:       admissionBody("test-model"),
			setup:      func(world *admissionWorld) { world.seedAlias("test-model", 4096, 10) },
			wantReason: execution.RejectedInvalidRequest,
			wantDetail: DetailNone,
		},
		{
			name: "an eligible grant that cannot cover the hold",
			body: admissionBody("test-model"),
			setup: func(world *admissionWorld) {
				world.buckets = nil
				world.seedBucket(admissionAccount, "bucket-small", 1, true)
			},
			wantReason: execution.RejectedInsufficientEntitlement,
			wantDetail: DetailNone,
		},
		{
			name: "no grant eligible to fund the alias",
			body: admissionBody("test-model"),
			setup: func(world *admissionWorld) {
				world.buckets = nil
				world.seedBucket(admissionAccount, "bucket-foreign", 10_000, false)
			},
			wantReason: execution.RejectedNoAccess,
			wantDetail: DetailNone,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCase, world := admissionFixture(t)
			if tt.setup != nil {
				tt.setup(world)
			}

			outcome, err := useCase.Serve(context.Background(), admissionInput(tt.body))
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != OutcomeRejected || outcome.Reason != tt.wantReason || outcome.Detail != tt.wantDetail {
				t.Fatalf("outcome = %s/%s/%q, want rejected/%s/%q", outcome.Kind, outcome.Reason, outcome.Detail, tt.wantReason, tt.wantDetail)
			}

			// One committed unit, no rollback: the refusal is a decision, and a
			// decision is on the record.
			if world.commitCount() != 1 || world.rollbackCount() != 0 {
				t.Fatalf("the refusal committed %d units and rolled back %d, want one commit and no rollback", world.commitCount(), world.rollbackCount())
			}

			// The pair: the row first, the replay record born terminal second.
			pairSeen := false
			for i, step := range world.events {
				if step == "request.insert" && i+1 < len(world.events) && world.events[i+1] == "intake.insert" {
					pairSeen = true
				}
			}
			if !pairSeen {
				t.Fatalf("the refusal did not write the pair in order: %v", world.events)
			}

			row := admissionRequestRow(t, world)
			if row.Status != execution.StatusRejected || row.RejectionReason != tt.wantReason {
				t.Fatalf("the row is %s/%s, want rejected/%s", row.Status, row.RejectionReason, tt.wantReason)
			}
			intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1")
			if intake.FinalStatus == nil || *intake.FinalStatus != execution.FinalRejected || intake.FinalRejectionReason != tt.wantReason {
				t.Fatalf("the replay record is not born terminal with the deciding reason")
			}
			if len(world.facts) != 0 {
				t.Fatalf("a refusal appended a fact, want none — nothing was released or settled")
			}
		})
	}
}

// TestAdmissionRefusesTheHoldBeforeTheWaterfall: an oversized or overflowing
// hold never reaches the ledger — the refusal happens on the request's own
// arithmetic, and no capacity moves.
func TestAdmissionRefusesTheHoldBeforeTheWaterfall(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(world *admissionWorld)
	}{
		{
			name: "a hold that overflows",
			setup: func(world *admissionWorld) {
				world.seedPrice("alias-test-model", catalog.PriceSnapshot{
					RevisionID: "rev-big", InputUnitPrice: math.MaxInt64, OutputUnitPrice: math.MaxInt64,
				})
			},
		},
		{
			name:  "a hold past the alias's cap",
			setup: func(world *admissionWorld) { world.seedAlias("test-model", 4096, 10) },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCase, world := admissionFixture(t)
			tt.setup(world)

			outcome, err := useCase.Serve(context.Background(), admissionInput(admissionBody("test-model")))
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != OutcomeRejected || outcome.Reason != execution.RejectedInvalidRequest || outcome.Detail != DetailNone {
				t.Fatalf("outcome = %s/%s/%q, want rejected/invalid_request/none", outcome.Kind, outcome.Reason, outcome.Detail)
			}
			if world.happened("ledger.drawdown") {
				t.Fatalf("the waterfall was asked to fund a hold the request was refused for")
			}
		})
	}
}

// TestAdmissionAcceptsAHoldAtExactlyTheCap: the cap bounds a single request's
// hold, and a hold AT the cap is admissible — the boundary is inclusive.
func TestAdmissionAcceptsAHoldAtExactlyTheCap(t *testing.T) {
	useCase, world := admissionFixture(t)
	world.seedAlias("test-model", 4096, 103) // the default body draws exactly 103

	outcome, err := useCase.Serve(context.Background(), admissionInput(admissionBody("test-model")))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeAdmitted {
		t.Fatalf("outcome = %s, want admitted", outcome.Kind)
	}
	if !world.happened("ledger.drawdown") {
		t.Fatalf("the hold at the cap was refused, want it drawn")
	}
}

// TestAdmissionWritesNothingWhenTheAliasIsUnpriced: an unpriced alias is the
// one interior outcome that writes nothing — an operator's unfinished
// configuration is nobody's refusal to record.
func TestAdmissionWritesNothingWhenTheAliasIsUnpriced(t *testing.T) {
	useCase, world := admissionFixture(t)
	world.priceMissing = true

	_, err := useCase.Serve(context.Background(), admissionInput(admissionBody("test-model")))
	if err == nil {
		t.Fatalf("Serve answered an unpriced alias, want the internal failure")
	}
	if world.commitCount() != 0 || world.rollbackCount() != 1 {
		t.Fatalf("the unpriced alias committed %d and rolled back %d, want one rollback and no commit", world.commitCount(), world.rollbackCount())
	}
	if len(world.requests) != 0 || len(world.intakes) != 0 || len(world.reservations) != 0 {
		t.Fatalf("the rollback left rows behind: %d requests, %d records, %d holds", len(world.requests), len(world.intakes), len(world.reservations))
	}
	if got := world.available("bucket-1"); got != 10_000 {
		t.Fatalf("the grant moved to %d, want 10000 — nothing was drawn", got)
	}
}

// ---------------------------------------------------------------------------
// the retry policies
// ---------------------------------------------------------------------------

// TestAdmissionAnswersTheUniqueKeyRaceOutsideTheAbortedUnit: when the replay
// record's insert loses its unique key, the answer is re-read OUTSIDE the
// aborted unit — the record that actually committed decides, and no caller
// ever sees the race as an internal failure.
func TestAdmissionAnswersTheUniqueKeyRaceOutsideTheAbortedUnit(t *testing.T) {
	for _, tt := range []struct {
		name       string
		winner     string
		wantKind   OutcomeKind
		wantReason execution.RejectionReason
		wantOrig   identity.RequestID
		freshRetry bool
	}{
		{
			name:       "a concurrent rejection answers itself again",
			winner:     "rejected",
			wantKind:   OutcomeReplay,
			wantReason: execution.RejectedAccountSuspended,
			wantOrig:   "winner-rejected",
		},
		{
			name:     "a concurrent admission answers in flight",
			winner:   "in_flight",
			wantKind: OutcomeInFlight,
		},
		{
			name:       "a winner that aborted too leaves the field to a fresh attempt",
			winner:     "",
			wantKind:   OutcomeAdmitted,
			wantReason: "",
			freshRetry: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCase, world := admissionFixture(t)
			world.intakeRace = 1
			world.intakeRaceWinner = tt.winner

			outcome, err := useCase.Serve(context.Background(), admissionInput(admissionBody("test-model")))
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != tt.wantKind || outcome.Reason != tt.wantReason {
				t.Fatalf("outcome = %s/%s, want %s/%s", outcome.Kind, outcome.Reason, tt.wantKind, tt.wantReason)
			}
			if outcome.Original != tt.wantOrig {
				t.Fatalf("outcome original = %q, want %q", outcome.Original, tt.wantOrig)
			}
			if !world.happened("rollback") {
				t.Fatalf("the raced unit did not roll back")
			}
			if tt.freshRetry {
				row := admissionRequestRow(t, world)
				if row.ID == identity.RequestID("req-transport-1") {
					t.Fatalf("the retry reused the aborted attempt's identity, want a fresh one")
				}
			}
		})
	}
}

// TestAdmissionRetriesTheUnitOnlyOnContention: the whole admission unit
// retries on the engine's contention classes and on nothing else — not on a
// constraint it violated, and not on the caller walking away.
func TestAdmissionRetriesTheUnitOnlyOnContention(t *testing.T) {
	for _, tt := range []struct {
		name     string
		code     string
		failures int
		wantErr  bool
	}{
		{name: "a serialization failure is retried and then admitted", code: "40001", failures: 2},
		{name: "a deadlock is retried and then admitted", code: "40P01", failures: 1},
		{name: "a dropped connection is retried and then admitted", code: "08003", failures: 1},
		{name: "a constraint the request itself violated is not retried", code: "23514", failures: 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCase, world := admissionFixture(t)
			world.drawdownFailure = fakePGError{code: tt.code}
			world.drawdownFailures = tt.failures

			outcome, err := useCase.Serve(context.Background(), admissionInput(admissionBody("test-model")))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Serve answered a unit that failed on its merits, want the failure")
				}
				if world.commitCount() != 0 {
					t.Fatalf("the failed unit committed")
				}
				return
			}
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != OutcomeAdmitted {
				t.Fatalf("outcome = %s, want admitted after the retried unit", outcome.Kind)
			}
			if world.rollbackCount() != tt.failures {
				t.Fatalf("the world rolled back %d units, want %d", world.rollbackCount(), tt.failures)
			}
		})
	}
}

// TestAdmissionNeverRetriesACallerWhoLeft: a cancelled context is not a
// contention class — the caller is gone and the budget belongs to it.
func TestAdmissionNeverRetriesACallerWhoLeft(t *testing.T) {
	useCase, world := admissionFixture(t)
	world.drawdownFailure = context.Canceled
	world.drawdownFailures = 1

	if _, err := useCase.Serve(context.Background(), admissionInput(admissionBody("test-model"))); err == nil {
		t.Fatalf("Serve answered a request whose caller had left, want the failure")
	}
	if world.rollbackCount() != 1 {
		t.Fatalf("the unit ran %d times, want once", world.rollbackCount())
	}
}

// ---------------------------------------------------------------------------
// the guards
// ---------------------------------------------------------------------------

// TestAdmissionRefusesToRunInsideSomeoneElsesUnitOfWork: admission opens its
// own units; a caller already inside one is a wiring defect, and joining it
// would commit their work with ours.
func TestAdmissionRefusesToRunInsideSomeoneElsesUnitOfWork(t *testing.T) {
	useCase, world := admissionFixture(t)
	ctx := context.WithValue(context.Background(), admissionTxKey{}, true)

	if _, err := useCase.Serve(ctx, admissionInput(admissionBody("test-model"))); err == nil {
		t.Fatalf("Serve joined a unit of work it did not open")
	}
	if len(world.events) != 0 {
		t.Fatalf("the guard wrote %v, want nothing", world.events)
	}
}

// TestAdmissionDuplicateHoldIsABugNotAnAnswer: a duplicate hold behind the
// probe and the unique key is a defect, and it is failed loudly rather than
// converted into an answer.
func TestAdmissionDuplicateHoldIsABugNotAnAnswer(t *testing.T) {
	useCase, world := admissionFixture(t)
	world.reservationDuplicate = true

	if _, err := useCase.Serve(context.Background(), admissionInput(admissionBody("test-model"))); err == nil {
		t.Fatalf("Serve answered a duplicate hold, want the internal failure")
	}
	if world.rollbackCount() != 1 || world.commitCount() != 0 {
		t.Fatalf("the duplicate hold committed %d and rolled back %d, want one rollback and no commit", world.commitCount(), world.rollbackCount())
	}
	if len(world.intakes) != 0 {
		t.Fatalf("the aborted unit left a replay record behind")
	}
}

// TestIsRetryableStoreFailure is the classifier's own table: the engine's
// contention classes and the connection exceptions retry, everything else
// fails on its merits, and a caller's cancelled context is never anyone's
// retry.
func TestIsRetryableStoreFailure(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "serialization failure", err: fakePGError{code: "40001"}, want: true},
		{name: "deadlock", err: fakePGError{code: "40P01"}, want: true},
		{name: "connection exception", err: fakePGError{code: "08006"}, want: true},
		{name: "connection exception, wrapped", err: fmt.Errorf("application: admit: %w", fakePGError{code: "08003"}), want: true},
		{name: "unique violation", err: fakePGError{code: "23505"}},
		{name: "check violation", err: fakePGError{code: "23514"}},
		{name: "a failure with no sqlstate at all", err: errors.New("fake: the socket vanished")},
		{name: "the caller cancelled", err: context.Canceled},
		{name: "the caller's deadline passed", err: context.DeadlineExceeded},
		{name: "no error at all", err: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableStoreFailure(tt.err); got != tt.want {
				t.Fatalf("isRetryableStoreFailure = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAdmissionParsesTheCeilingsSpellingsDirectly: the bounds are judged
// against the alias they are bounded by, and the alias refusal outranks the
// bounds refusals — an unknown model never reaches the ceiling grammar, even
// when the ceiling is broken too.
func TestAdmissionParsesTheCeilingsSpellingsDirectly(t *testing.T) {
	useCase, world := admissionFixture(t)
	body := []byte(`{"model":"no-such-model","max_tokens":"broken","messages":[{"content":"hello"}]}`)

	outcome, err := useCase.Serve(context.Background(), admissionInput(body))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Reason != execution.RejectedUnknownAlias || outcome.Detail != DetailNone {
		t.Fatalf("outcome = %s/%q, want unknown_alias with no detail — the alias refusal outranks the bounds", outcome.Reason, outcome.Detail)
	}
	if world.happened("ledger.drawdown") {
		t.Fatalf("an unknown alias reached the waterfall")
	}
}

// ---------------------------------------------------------------------------
// the replayed refusal's field detail
// ---------------------------------------------------------------------------

// TestAdmissionReplayReDerivesTheFieldDetail: a replayed invalid_request
// re-derives the field it names from the bytes the digest matched — the
// record carries the reason, never the field — and the derivation degrades to
// no detail rather than inventing one it cannot support. Nothing is written:
// the decision is already on the record, only its field name is re-judged.
func TestAdmissionReplayReDerivesTheFieldDetail(t *testing.T) {
	for _, tt := range []struct {
		name       string
		body       []byte
		wantDetail RejectionDetail
	}{
		{
			name:       "a ceiling past the alias's own bound names that ceiling",
			body:       []byte(`{"model":"test-model","max_tokens":5000,"messages":[{"content":"hello"}]}`),
			wantDetail: DetailMaxTokens,
		},
		{
			name:       "a ceiling past the other spelling's bound names the other spelling",
			body:       []byte(`{"model":"test-model","max_completion_tokens":5000,"messages":[{"content":"hello"}]}`),
			wantDetail: DetailMaxCompletionTokens,
		},
		{
			name:       "a body with no model names the model",
			body:       []byte(`{"max_tokens":16,"messages":[{"content":"hello"}]}`),
			wantDetail: DetailModel,
		},
		{
			name:       "a body that will not parse degrades to no detail",
			body:       []byte("{not json"),
			wantDetail: DetailNone,
		},
		{
			name:       "a model the catalog has lost degrades to no detail",
			body:       admissionBody("gone-model"),
			wantDetail: DetailNone,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCase, world := admissionFixture(t)
			digest := execution.SecretDigest(tt.body)
			seedReplayRecord(world, digest, finalStatusPtr(execution.FinalRejected), execution.RejectedInvalidRequest, "original-1")

			outcome, err := useCase.Serve(context.Background(), admissionInput(tt.body))
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != OutcomeReplay || outcome.Reason != execution.RejectedInvalidRequest {
				t.Fatalf("outcome = %s/%s, want replay/invalid_request", outcome.Kind, outcome.Reason)
			}
			if outcome.Detail != tt.wantDetail {
				t.Fatalf("replayed detail = %q, want %q", outcome.Detail, tt.wantDetail)
			}
			if outcome.Original != "original-1" {
				t.Fatalf("replayed original = %q, want the original's own id", outcome.Original)
			}
			if len(world.events) != 0 {
				t.Fatalf("the re-derivation wrote %v, want nothing", world.events)
			}
		})
	}
}

// TestAdmissionReplayOfANonFieldRefusalCarriesNoDetail: only an
// invalid_request replay may carry a field name. A refusal about the account
// or the alias is replayed with its reason alone, even when the replayed body
// would fault a ceiling if it were judged — the original's decision, not a
// re-run of it, is what a replay answers.
func TestAdmissionReplayOfANonFieldRefusalCarriesNoDetail(t *testing.T) {
	useCase, world := admissionFixture(t)
	body := []byte(`{"model":"test-model","max_tokens":5000,"messages":[{"content":"hello"}]}`)
	seedReplayRecord(world, execution.SecretDigest(body), finalStatusPtr(execution.FinalRejected), execution.RejectedAccountSuspended, "original-1")

	outcome, err := useCase.Serve(context.Background(), admissionInput(body))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeReplay || outcome.Reason != execution.RejectedAccountSuspended {
		t.Fatalf("outcome = %s/%s, want replay/account_suspended", outcome.Kind, outcome.Reason)
	}
	if outcome.Detail != DetailNone {
		t.Fatalf("replayed detail = %q, want none — the account refusal names no request field", outcome.Detail)
	}
	if len(world.events) != 0 {
		t.Fatalf("the replay wrote %v, want nothing", world.events)
	}
}

// ---------------------------------------------------------------------------
// retired aliases
// ---------------------------------------------------------------------------

// TestAdmissionRefusesARetiredAliasAsUnknown: retirement takes a name out of
// resolution one-way, so the runtime answers unknown_alias for it exactly as
// it would for a name no row ever carried — and the refusal is a decision,
// so it is recorded as the pair and replayed from the record after it.
func TestAdmissionRefusesARetiredAliasAsUnknown(t *testing.T) {
	useCase, world := admissionFixture(t)
	world.aliasesByName["test-model"].State = catalog.AliasRetired

	outcome, err := useCase.Serve(context.Background(), admissionInput(admissionBody("test-model")))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeRejected || outcome.Reason != execution.RejectedUnknownAlias {
		t.Fatalf("outcome = %s/%s, want rejected/unknown_alias", outcome.Kind, outcome.Reason)
	}
	wantEvents(t, world, []string{"begin", "request.insert", "intake.insert", "commit"})
	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusRejected || row.RejectionReason != execution.RejectedUnknownAlias || row.Alias != "test-model" {
		t.Fatalf("the row is %s/%s for alias %q, want rejected/unknown_alias naming the retired name", row.Status, row.RejectionReason, row.Alias)
	}
	intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1")
	if intake.FinalStatus == nil || *intake.FinalStatus != execution.FinalRejected || intake.FinalRejectionReason != execution.RejectedUnknownAlias {
		t.Fatalf("the replay record is not born terminal with unknown_alias")
	}
	if world.happened("ledger.drawdown") {
		t.Fatalf("a retired alias reached the waterfall")
	}
}

// ---------------------------------------------------------------------------
// the refusal path's own race
// ---------------------------------------------------------------------------

// TestAdmissionRefusalThatLosesTheIntakeRaceAnswersTheWinner: a refusal
// decided inside the unit can lose the record's unique key to a concurrent
// unit, exactly as the admission path can. The losing unit aborts whole and
// the winner's committed decision is what the caller is answered with — the
// loser's own refusal is not half-committed and never surfaces.
func TestAdmissionRefusalThatLosesTheIntakeRaceAnswersTheWinner(t *testing.T) {
	useCase, world := admissionFixture(t)
	world.intakeRace = 1
	world.intakeRaceWinner = "rejected"
	// The body refuses on its ceiling, so the unit decides a refusal and
	// reaches the record's insert with it.
	body := []byte(`{"model":"test-model","max_tokens":5000,"messages":[{"content":"hello"}]}`)

	outcome, err := useCase.Serve(context.Background(), admissionInput(body))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeReplay || outcome.Reason != execution.RejectedAccountSuspended {
		t.Fatalf("outcome = %s/%s, want the winner's replay/rejected", outcome.Kind, outcome.Reason)
	}
	if outcome.Original != "winner-rejected" {
		t.Fatalf("replayed original = %q, want the winner's request id", outcome.Original)
	}
	if outcome.Detail != DetailNone {
		t.Fatalf("replayed detail = %q, want none — the winner's refusal names no request field", outcome.Detail)
	}
	// The losing unit ran to the record's insert and was aborted whole; the
	// re-probe that answered the winner wrote nothing.
	wantEvents(t, world, []string{"begin", "request.insert", "intake.insert", "rollback"})
	if world.commitCount() != 0 {
		t.Fatalf("the loser committed %d units, want none", world.commitCount())
	}
	if len(world.facts) != 0 {
		t.Fatalf("the loser appended a fact, want none")
	}
	if world.outsideTx != 0 {
		t.Fatalf("%d calls ran outside a unit of work", world.outsideTx)
	}
}

// ---------------------------------------------------------------------------
// the seam's own doctrine
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// the constructor's own bounds
// ---------------------------------------------------------------------------

// TestNewChatAdmissionRequiresALeaseInsideTheHoldWindow: a lease that
// outlives the hold it fences lets a process close a hold the reaper already
// owns — the constructor refuses the wiring rather than running it.
func TestNewChatAdmissionRequiresALeaseInsideTheHoldWindow(t *testing.T) {
	for _, tt := range []struct {
		name      string
		hold      time.Duration
		lease     time.Duration
		wantPanic bool
	}{
		{name: "a lease inside its hold window", hold: time.Minute, lease: 30 * time.Second},
		{name: "a lease at the hold window's edge is still inside it", hold: time.Minute, lease: time.Minute},
		{name: "a lease past its hold window", hold: 30 * time.Second, lease: time.Minute, wantPanic: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if tt.wantPanic && recovered == nil {
					t.Fatalf("NewChatAdmission accepted a lease TTL past its hold window")
				}
				if !tt.wantPanic && recovered != nil {
					t.Fatalf("NewChatAdmission panicked on %s: %v", tt.name, recovered)
				}
			}()
			newAdmissionWithHorizons(t, tt.hold, tt.lease)
		})
	}
}

// newAdmissionWithHorizons builds the use case over one world with the
// horizons given — the same wiring every admission test uses, with only the
// clock's fences moved.
func newAdmissionWithHorizons(t *testing.T, hold, lease time.Duration) *ChatAdmission {
	t.Helper()
	world := newAdmissionWorld()
	useCase := NewChatAdmission(
		fakeAdmissionStore{world: world},
		fakeAdmissionCredentials{world: world},
		fakeAdmissionAliases{world: world},
		fakeAdmissionPrices{world: world},
		fakeAdmissionRequests{world: world},
		fakeAdmissionIntakes{world: world},
		fakeAdmissionReservations{world: world},
		fakeAdmissionLedger{world: world},
		fakeAdmissionFacts{world: world},
		AdmissionConfig{HoldWindow: hold, LeaseTTL: lease, LeaseOwner: "test-host:1"},
	)
	useCase.clock = admissionClock{world}
	return useCase
}

// ---------------------------------------------------------------------------
// the canonical v1 count, at admission
// ---------------------------------------------------------------------------

// TestParseCountsTheWholeBodyInBytes: the input count prices the hold, and
// the canonical v1 rule counts the WHOLE admitted body in bytes — not runes,
// and not the message strings alone. A body whose envelope dwarfs its
// content holds against the request the provider will actually tokenize, and
// a multibyte script holds against its real wire size.
func TestParseCountsTheWholeBodyInBytes(t *testing.T) {
	raw := []byte(`{"model":"test-model","max_tokens":16,"messages":[` +
		`{"content":"hello"},` +
		`{"content":"你好世界"},` +
		`{"role":"assistant"},` +
		`{"content":""}` +
		`],"stream":false}`)
	parsed, refusal := parseChatRequest(raw)
	if refusal != nil {
		t.Fatalf("a well-formed body refused: %s", refusal.reason)
	}
	// The count is the body's own byte length. The content strings inside it
	// are only 17 of those bytes — "hello" is 5, "你好世界" is 12 (4 runes ×
	// 3), not 4 — so a count that landed at or below 17 would be reading the
	// message strings alone, the exact undercount the whole-body rule
	// replaced, and a rune count would halve the multibyte part outright.
	if parsed.inputTokens != int64(len(raw)) {
		t.Fatalf("input tokens = %d, want the body's %d bytes — the count is the whole body, in bytes", parsed.inputTokens, len(raw))
	}
	if parsed.inputTokens <= 17 {
		t.Fatalf("input tokens = %d, at or below the content strings' 17 bytes; the envelope must be counted", parsed.inputTokens)
	}
}

// TestParseCountsTheToolsTheProviderBills pins the scope's reason: tools and
// their schemas are input a provider tokenizes and bills, and the body that
// carries them must price a bigger hold than the same conversation without
// them. The count that read only the messages would size both holds alike
// and under-cover the second.
func TestParseCountsTheToolsTheProviderBills(t *testing.T) {
	plain := []byte(`{"model":"test-model","max_tokens":16,"messages":[{"content":"hi"}]}`)
	withTools := []byte(`{"model":"test-model","max_tokens":16,"messages":[{"content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"search","description":"search the web",` +
		`"parameters":{"type":"object","properties":{"query":{"type":"string"}}}}}]}`)

	bareParsed, refusal := parseChatRequest(plain)
	if refusal != nil {
		t.Fatalf("the plain body refused: %s", refusal.reason)
	}
	tooledParsed, refusal := parseChatRequest(withTools)
	if refusal != nil {
		t.Fatalf("the tools-bearing body refused: %s", refusal.reason)
	}
	if tooledParsed.inputTokens <= bareParsed.inputTokens {
		t.Fatalf("input tokens = %d with tools and %d without; the envelope the provider bills must be counted", tooledParsed.inputTokens, bareParsed.inputTokens)
	}
}
