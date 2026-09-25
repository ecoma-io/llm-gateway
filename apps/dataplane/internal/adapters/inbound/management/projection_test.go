package management

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	stdhttp "net/http"
	"net/http/httptest"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
)

// The projection handlers' tests: what the wire does with a delivered body —
// what reaches the application as a parsed message, what is refused before the
// application is touched, and what the answer bytes look like. The protocol
// agreement itself is protocol_test.go's; the grammar is the domain's; what is
// pinned here is this hop's translation of both, including the sentence that a
// refused delivery never half-arrives.

// authenticated builds a request for a delivered operation carrying the
// configured credential, with a JSON body when one is wanted.
func authenticated(t *testing.T, method, target, body string) *stdhttp.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set(authorizationHeader, credentialScheme+" "+serviceCredential)
	return request
}

func TestThePositionAnswerIsTheStoredPositionVerbatim(t *testing.T) {
	// The position is this plane's own fact, and the answer bytes are the
	// whole of the operation: the producer's loop decides from exactly these
	// fields and nothing else, so the body is pinned as bytes rather than
	// decoded and inspected — the same contract-surface discipline the fact
	// feed's tests hold.
	applier := &stubProjection{position: projection.Position{
		Epoch:           "0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90",
		Bootstrapped:    true,
		AppliedRevision: 41,
	}}

	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodGet, "/internal/projection/position", ""))

	want := `{"protocol_version":1,"epoch":"0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90",` +
		`"bootstrapped":true,"applied_revision":41}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	if rec.Code != stdhttp.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, stdhttp.StatusOK)
	}
}

// TestTheUnbootstrappedPositionSaysSo pins the shape that makes bootstrap
// decidable: a store that has never applied a snapshot answers with an empty
// epoch and revision zero, and the producer reads that as "offer a snapshot,
// never a batch".
func TestTheUnbootstrappedPositionSaysSo(t *testing.T) {
	applier := &stubProjection{}

	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodGet, "/internal/projection/position", ""))

	want := `{"protocol_version":1,"epoch":"","bootstrapped":false,"applied_revision":0}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestThePositionMapsAStoreFailureToInternal(t *testing.T) {
	applier := &stubProjection{positionErr: context.DeadlineExceeded}

	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodGet, "/internal/projection/position", ""))

	if rec.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusInternalServerError)
	}
	if got := envelopeCode(t, rec); got != codeInternal {
		t.Errorf("error code = %q, want %q", got, codeInternal)
	}
}

// snapshotBody builds a snapshot request body: one credential, one account,
// and whatever overrides the caller passes.
func snapshotBody(t *testing.T, overrides ...func(map[string]any)) string {
	t.Helper()
	body := map[string]any{
		"protocol_version":  projection.ProtocolVersion,
		"epoch":             "0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90",
		"snapshot_revision": 7,
		"api_keys": []map[string]any{{
			"key_id":     "11111111-2222-4333-8444-555555555555",
			"account_id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
			"digest":     strings.Repeat("b", 64),
			"state":      "active",
			"revoked_at": nil,
		}},
		"accounts": []map[string]any{{
			"account_id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
			"state":      "active",
		}},
	}
	for _, override := range overrides {
		override(body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("building a snapshot body: %v", err)
	}
	return string(raw)
}

func TestASnapshotIsParsedAndHandedToTheApplication(t *testing.T) {
	applier := &stubProjection{ack: 7}

	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/snapshot", snapshotBody(t)))

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}
	want := `{"protocol_version":1,"applied_revision":7}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	if len(applier.snapshots) != 1 {
		t.Fatalf("the application received %d snapshots, want 1", len(applier.snapshots))
	}
	snapshot := applier.snapshots[0]
	if snapshot.SnapshotRevision != 7 || snapshot.Epoch != "0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90" {
		t.Errorf("the snapshot's boundary moved: %+v", snapshot)
	}
	if len(snapshot.APIKeys) != 1 || snapshot.APIKeys[0].Digest != strings.Repeat("b", 64) || snapshot.APIKeys[0].State != projection.CredentialActive {
		t.Errorf("the credential record did not survive the parse: %+v", snapshot.APIKeys)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].State != projection.LifecycleActive {
		t.Errorf("the account record did not survive the parse: %+v", snapshot.Accounts)
	}
}

func TestASnapshotOutsideTheGrammarIsRefusedBeforeTheApplication(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "a version this plane does not speak",
			body: snapshotBody(t, func(body map[string]any) { body["protocol_version"] = 2 }),
		},
		{
			name: "no epoch at all",
			body: snapshotBody(t, func(body map[string]any) { delete(body, "epoch") }),
		},
		{
			name: "a credential whose digest is null",
			body: snapshotBody(t, func(body map[string]any) {
				body["api_keys"] = []map[string]any{{
					"key_id":     "11111111-2222-4333-8444-555555555555",
					"account_id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
					"digest":     nil,
					"state":      "active",
					"revoked_at": nil,
				}}
			}),
		},
		{
			name: "a credential revoked without an instant",
			body: snapshotBody(t, func(body map[string]any) {
				body["api_keys"] = []map[string]any{{
					"key_id":     "11111111-2222-4333-8444-555555555555",
					"account_id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
					"digest":     strings.Repeat("b", 64),
					"state":      "revoked",
					"revoked_at": nil,
				}}
			}),
		},
		{
			name: "an account in a state the lifecycle does not name",
			body: snapshotBody(t, func(body map[string]any) {
				body["accounts"] = []map[string]any{{"account_id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "state": "archived"}}
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applier := &stubProjection{}
			rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/snapshot", tt.body))

			if rec.Code != stdhttp.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
			}
			if got := envelopeCode(t, rec); got != codeInvalidRequest && got != codeUnsupportedVersion {
				t.Errorf("error code = %q, want a refusal in this surface's vocabulary", got)
			}
			if len(applier.snapshots) != 0 {
				t.Errorf("the store received %d snapshots; a refused snapshot must not reach it", len(applier.snapshots))
			}
		})
	}
}

func TestASnapshotWithAdditiveFieldsIsDelivered(t *testing.T) {
	// The one additive direction the protocol evolves in: unknown fields on a
	// delivered message are tolerated and ignored, because a consumer that
	// upgrades first must not refuse the messages the producer still sends —
	// that ordering is the protocol's, and this handler is where it holds.
	body := snapshotBody(t, func(body map[string]any) { body["delivery_tag"] = "first-of-the-day" })

	applier := &stubProjection{ack: 7}
	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/snapshot", body))

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}
	if len(applier.snapshots) != 1 {
		t.Errorf("the store received %d snapshots, want the additive-field snapshot delivered", len(applier.snapshots))
	}
}

func TestARevisionZeroChangeIsRefused(t *testing.T) {
	// Revisions start at one; a zero is a producer that lost its arithmetic,
	// and the guard arithmetic that decides "behind or joining" cannot mean
	// anything about it.
	body := `{"protocol_version":1,"epoch":"0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90",` +
		`"from_revision":0,"changes":[{"revision":0,"resource_kind":"account",` +
		`"resource_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",` +
		`"recorded_at":"2026-09-24T10:00:00Z","payload":{"state":"active"}}]}`

	applier := &stubProjection{}
	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/changes", body))

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
	}
	if len(applier.batches) != 0 {
		t.Errorf("the store received %d batches; a refused batch must not reach it", len(applier.batches))
	}
}

func TestABatchIsParsedEntryByEntryAndRefusedWhole(t *testing.T) {
	// One bad entry among good ones refuses the batch at the grammar stage —
	// the entries are parsed as they are read, and the first refusal stops the
	// handover. Nothing reaches the application half-parsed.
	body := `{"protocol_version":1,"epoch":"0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90",` +
		`"from_revision":10,"changes":[` +
		`{"revision":11,"resource_kind":"account","resource_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","recorded_at":"2026-09-24T10:00:00Z","payload":{"state":"active"}},` +
		`{"revision":12,"resource_kind":"api_key","resource_id":"11111111-2222-4333-8444-555555555555","recorded_at":"2026-09-24T10:00:00Z","payload":{"account_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","digest":"nothex","state":"active","revoked_at":null}}]}`

	applier := &stubProjection{}
	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/changes", body))

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
	}
	if len(applier.batches) != 0 {
		t.Errorf("the store received %d batches; a batch with one bad entry must not reach it", len(applier.batches))
	}
}

func TestAGrammarValidBatchIsHandedToTheApplicationParsed(t *testing.T) {
	applier := &stubProjection{ack: 11}

	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/changes", grammarValidBatchBody))

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}
	want := `{"protocol_version":1,"applied_revision":11}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	if len(applier.batches) != 1 {
		t.Fatalf("the application received %d batches, want 1", len(applier.batches))
	}
	batch := applier.batches[0]
	if batch.FromRevision != 10 || batch.Epoch != "0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90" {
		t.Errorf("the batch's envelope moved: %+v", batch)
	}
	if len(batch.Changes) != 1 || batch.Changes[0].Account == nil || batch.Changes[0].Account.State != projection.LifecycleActive {
		t.Errorf("the change did not survive the parse: %+v", batch.Changes)
	}
	if got := applier.batches[0].Changes[0].RecordedAt; got.IsZero() {
		t.Errorf("the change lost its recording instant: %+v", got)
	}
}

func TestAnUnreadableBodyIsRefused(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "a body that is not JSON", body: "{not json"},
		{name: "a body that is not an object", body: "[1,2,3]"},
		{name: "an empty body", body: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applier := &stubProjection{}
			rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/changes", tt.body))

			if rec.Code != stdhttp.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
			}
			if got := envelopeMessage(t, rec); got != "the request body is not a message this operation can read" {
				t.Errorf("error message = %q, want the fixed sentence", got)
			}
			if len(applier.batches) != 0 {
				t.Errorf("the store received %d batches; an unreadable body must not reach it", len(applier.batches))
			}
		})
	}
}

func TestABodyPastTheContractCeilingIsRefused(t *testing.T) {
	// The ceiling is the contract's, and the refusal names no byte count of
	// the caller's body: a delivered body is untrusted input, and this
	// surface's error sentence is a decision, not an echo.
	body := `{"protocol_version":1,"epoch":"0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90",` +
		`"snapshot_revision":1,"api_keys":[],"accounts":[],"padding":"` +
		strings.Repeat("a", maxProjectionBodyBytes) + `"}`

	applier := &stubProjection{}
	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/snapshot", body))

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusBadRequest)
	}
	if got := envelopeMessage(t, rec); got != "the request body is larger than this operation accepts" {
		t.Errorf("error message = %q, want the fixed sentence", got)
	}
	if len(applier.snapshots) != 0 {
		t.Errorf("the store received %d snapshots; an oversize body must not reach it", len(applier.snapshots))
	}
}

// TestAnOversizeBatchAgainstTheCeilingIsRefused is the grammar's bound, the
// contract's maxItems, proven at the wire: one more change than the batch
// bound carries is refused before the application is asked to judge it.
func TestAnOversizeBatchAgainstTheCeilingIsRefused(t *testing.T) {
	var entries []string
	for revision := 1; revision <= projection.MaxChangesPerBatch+1; revision++ {
		entries = append(entries, `{"revision":`+strconv.Itoa(revision)+`,"resource_kind":"account",`+
			`"resource_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",`+
			`"recorded_at":"2026-09-24T10:00:00Z","payload":{"state":"active"}}`)
	}
	body := `{"protocol_version":1,"epoch":"0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90",` +
		`"from_revision":0,"changes":[` + strings.Join(entries, ",") + `]}`

	applier := &stubProjection{}
	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/changes", body))

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusBadRequest)
	}
	if len(applier.batches) != 0 {
		t.Errorf("the store received %d batches; an oversize batch must not reach it", len(applier.batches))
	}
}

func TestAProjectionRefusalNamesNoDetail(t *testing.T) {
	// The position's numbers are this plane's facts about its mirror, and the
	// producer's loop learns them from the position operation — not from a
	// refusal body. The gap answer is a decision, and the decision's sentence
	// is all the wire carries.
	applier := &stubProjection{err: projection.ErrRevisionGap}

	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/changes", grammarValidBatchBody))

	if rec.Code != stdhttp.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusConflict)
	}
	want := "the delivered batch does not join the applied position"
	if got := envelopeMessage(t, rec); got != want {
		t.Errorf("error message = %q, want %q", got, want)
	}
	// The message equality above is the no-detail pin; this repeat over the
	// message spells it out. The whole body is deliberately not searched: its
	// request_id is a random hex string that can coincide with any digit pair.
	if strings.Contains(want, "11") || strings.Contains(want, "10") {
		t.Errorf("error message = %q: the refusal must not name the revisions it judged", want)
	}
}

// TestATerminalRegressionRefusalKeepsTheShapeVocabulary pins the fifth
// refusal's answer on the wire: it shares the shape refusal's status and
// code — the contract's envelope has no fifth projection code, and the
// caller's reflex is the invalid-request one, stop sending this message —
// while its sentence is its own, because the remediation is not "fix your
// grammar" but "investigate what produced this entry".
func TestATerminalRegressionRefusalKeepsTheShapeVocabulary(t *testing.T) {
	applier := &stubProjection{err: projection.ErrTerminalRegression}

	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/changes", grammarValidBatchBody))

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusBadRequest)
	}
	if got := envelopeCode(t, rec); got != codeInvalidRequest {
		t.Errorf("error code = %q, want %q", got, codeInvalidRequest)
	}
	want := "the delivered change would move a terminal state backwards"
	if got := envelopeMessage(t, rec); got != want {
		t.Errorf("error message = %q, want %q", got, want)
	}
}

func TestASnapshotRequiredRefusalNamesItsAnswer(t *testing.T) {
	applier := &stubProjection{err: projection.ErrSnapshotRequired}

	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/changes", grammarValidBatchBody))

	if rec.Code != stdhttp.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusConflict)
	}
	if got := envelopeCode(t, rec); got != codeSnapshotRequired {
		t.Errorf("error code = %q, want %q", got, codeSnapshotRequired)
	}
	if got := envelopeMessage(t, rec); !strings.Contains(got, "snapshot") {
		t.Errorf("error message = %q, want it to name the answer: a snapshot", got)
	}
}

// TestASnapshotMissingARequiredFieldIsRefusedBeforeTheApplication is the
// pointer half of the wire shapes: a snapshot's boundary and its two arrays
// are required, and their absence must be a refusal — not a silent decode
// into the legal empty form. An unconditional apply is the one operation that
// writes what it is told without judging it against the position, so a
// message that omitted half of itself and read as "empty at boundary 900"
// would advance the position past changes that are still to come, and their
// later deliveries would be duplicates forever.
//
// Each row deletes or nulls exactly one required field from a legal body, so
// a green run cannot come from some other refusal firing instead. The last
// row is the control: an explicit zero boundary with both arrays present and
// empty is the legitimate fresh-timeline snapshot, and must still be
// accepted.
func TestASnapshotMissingARequiredFieldIsRefusedBeforeTheApplication(t *testing.T) {
	tests := []struct {
		name     string
		override func(map[string]any)
	}{
		{
			name:     "no api_keys array",
			override: func(body map[string]any) { delete(body, "api_keys") },
		},
		{
			name:     "no accounts array",
			override: func(body map[string]any) { delete(body, "accounts") },
		},
		{
			name:     "no snapshot_revision",
			override: func(body map[string]any) { delete(body, "snapshot_revision") },
		},
		{
			name:     "a null api_keys array",
			override: func(body map[string]any) { body["api_keys"] = nil },
		},
		{
			name:     "a null accounts array",
			override: func(body map[string]any) { body["accounts"] = nil },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applier := &stubProjection{}
			body := snapshotBody(t, tt.override)

			rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/snapshot", body))

			if rec.Code != stdhttp.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
			}
			if got := envelopeCode(t, rec); got != codeInvalidRequest {
				t.Errorf("error code = %q, want %q", got, codeInvalidRequest)
			}
			if len(applier.snapshots) != 0 {
				t.Errorf("the store received %d snapshots; a snapshot that omitted a required field must not reach it", len(applier.snapshots))
			}
		})
	}

	t.Run("an explicit zero boundary with empty arrays is still a snapshot", func(t *testing.T) {
		applier := &stubProjection{}
		body := snapshotBody(t,
			func(body map[string]any) {
				body["snapshot_revision"] = 0
				body["api_keys"] = []map[string]any{}
				body["accounts"] = []map[string]any{}
			})

		rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/snapshot", body))

		if rec.Code != stdhttp.StatusOK {
			t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusOK, rec.Body.String())
		}
		if len(applier.snapshots) != 1 {
			t.Errorf("the store received %d snapshots, want the one legal empty snapshot", len(applier.snapshots))
		}
	})
}

// TestABatchWithAnUnsupportedVersionIsUnsupportedEvenWhenMalformed pins the
// refusal order: the version is judged before any entry is parsed, so a batch
// that names a version this plane does not speak is answered
// unsupported_version even when its entries are also garbage — which version
// refused a message must not depend on what else was wrong with it. The
// snapshot path judges first inside the domain's validation; the batch path's
// entries are parsed on the wire first, so its version check runs there.
func TestABatchWithAnUnsupportedVersionIsUnsupportedEvenWhenMalformed(t *testing.T) {
	applier := &stubProjection{}
	// Malformed at the grammar, legal at the JSON: every field type is one
	// the decoder accepts, and every value is one the domain's grammar
	// refuses — so the refusal that comes back can only be about order of
	// judgement, not about a body that could not decode at all.
	body := `{"protocol_version":99,"epoch":"not even a uuid","from_revision":4,"changes":[` +
		`{"revision":0,"resource_kind":"neither","resource_id":"garbage","recorded_at":"2026-09-25T09:00:00Z","payload":{"state":"active"}}` +
		`]}`

	rec := serveProjection(t, applier, authenticated(t, stdhttp.MethodPost, "/internal/projection/changes", body))

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
	}
	if got := envelopeCode(t, rec); got != codeUnsupportedVersion {
		t.Fatalf("error code = %q, want %q", got, codeUnsupportedVersion)
	}
	if len(applier.batches) != 0 {
		t.Errorf("the store received %d batches; an unsupported version must be refused before the application", len(applier.batches))
	}
}
