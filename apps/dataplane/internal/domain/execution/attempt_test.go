package execution

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// Every test here pins the attempt row's shape the database also enforces:
// the outcome/error-class pairing, the one capped object field, and the one
// sanctioned update — provider usage that arrives after the row exists and
// may fill nils but never overwrite a claim already written.

var (
	attemptStart = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	attemptEnd   = attemptStart.Add(3 * time.Second)
)

// allErrorClasses is the closed ADR 0002 vocabulary, spelled out so the
// pairing matrix below walks every value and a ninth class added later is
// this table's business, not its blind spot.
func allErrorClasses() []ErrorClass {
	return []ErrorClass{
		ErrorAuthentication, ErrorRateLimited, ErrorProviderUnavailable,
		ErrorProviderRejectedRequest, ErrorContextTooLarge, ErrorInvalidUpstreamResponse,
		ErrorUpstreamError, ErrorStreamAfterCommitment,
	}
}

// TestNewAttemptPairsOutcomeWithErrorClass walks the full pairing matrix the
// vocabulary allows and forbids: a succeeded attempt names no class (the
// database refuses one), a pre-commitment failure always names one (there is
// always a fault to charge), and a post-commitment failure may go either
// way — a broken stream has a fault, a departed client does not. Each
// mismatch must be refused with ErrAttemptOutcomePairing at formation, so
// the caller reads the disagreement it formed rather than a constraint
// violation it persisted.
func TestNewAttemptPairsOutcomeWithErrorClass(t *testing.T) {
	type pairing struct {
		name    string
		outcome Outcome
		class   ErrorClass
		wantErr error
	}
	cases := []pairing{
		{name: "succeeded without a class", outcome: OutcomeSucceeded},
		{name: "failed before commitment without a class", outcome: OutcomeFailedBeforeCommitment, wantErr: ErrAttemptOutcomePairing},
		{name: "failed after commitment without a class", outcome: OutcomeFailedAfterCommitment},
	}
	for _, class := range allErrorClasses() {
		cases = append(cases,
			pairing{name: "succeeded with " + string(class), outcome: OutcomeSucceeded, class: class, wantErr: ErrAttemptOutcomePairing},
			pairing{name: "failed before commitment with " + string(class), outcome: OutcomeFailedBeforeCommitment, class: class},
			pairing{name: "failed after commitment with " + string(class), outcome: OutcomeFailedAfterCommitment, class: class},
		)
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			attempt, err := NewAttempt("att-0001", "req-0001", 0, 1, "backend-anthropic", "claude-sonnet-4-5", tt.outcome, tt.class, attemptStart, attemptEnd)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("NewAttempt() error = %v, want the pairing accepted", err)
				}
				if attempt.Outcome != tt.outcome || attempt.ErrorClass != tt.class {
					t.Errorf("attempt = (%q, %q), want (%q, %q) — the pair is stored as formed", attempt.Outcome, attempt.ErrorClass, tt.outcome, tt.class)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("NewAttempt() error = %v, want ErrAttemptOutcomePairing", err)
			}
		})
	}
}

// TestNewAttemptRefusesAnUnusableCall pins each constructor refusal in
// isolation: identity, non-negative position counters, the backend/model
// pair, the closed vocabularies, and the caller-owned clock. The attempt is
// appended when the call finishes and never while in flight, so a caller
// that cannot say when the call ran has nothing to append; zero times are
// refused before a row can claim a call nobody timed.
func TestNewAttemptRefusesAnUnusableCall(t *testing.T) {
	// attemptArgs is the constructor's argument list under one name, so each
	// case can spoil exactly one argument of an otherwise valid call and
	// keep its refusal's cause unambiguous.
	type attemptArgs struct {
		id       identity.AttemptID
		request  identity.RequestID
		position int
		retry    int
		backend  string
		model    string
		outcome  Outcome
		class    ErrorClass
		start    time.Time
		end      time.Time
	}
	valid := func() attemptArgs {
		return attemptArgs{
			id: "att-0001", request: "req-0001", position: 0, retry: 1,
			backend: "backend-anthropic", model: "claude-sonnet-4-5",
			outcome: OutcomeFailedBeforeCommitment, class: ErrorRateLimited,
			start: attemptStart, end: attemptEnd,
		}
	}

	tests := []struct {
		name    string
		spoil   func(*attemptArgs)
		wantErr error // nil means the refusal is a plain error, pinned by message
		message string
	}{
		{
			name:    "rejects an empty attempt id",
			spoil:   func(a *attemptArgs) { a.id = "" },
			message: "needs its id and its request",
		},
		{
			name:    "rejects an empty request id",
			spoil:   func(a *attemptArgs) { a.request = "" },
			message: "needs its id and its request",
		},
		{
			name:    "rejects a negative candidate position",
			spoil:   func(a *attemptArgs) { a.position = -1 },
			message: "count from zero",
		},
		{
			name:    "rejects a negative retry sequence",
			spoil:   func(a *attemptArgs) { a.retry = -1 },
			message: "count from zero",
		},
		{
			name:    "rejects an empty backend",
			spoil:   func(a *attemptArgs) { a.backend = "" },
			message: "names its backend and provider model",
		},
		{
			name:    "rejects an empty provider model",
			spoil:   func(a *attemptArgs) { a.model = "" },
			message: "names its backend and provider model",
		},
		{
			name:    "rejects an unknown outcome",
			spoil:   func(a *attemptArgs) { a.outcome = "crashed" },
			message: "unknown attempt outcome",
		},
		{
			name:    "rejects an unknown error class",
			spoil:   func(a *attemptArgs) { a.class = "timeout" },
			wantErr: ErrUnknownErrorClass,
		},
		{
			name:    "rejects a zero started time",
			spoil:   func(a *attemptArgs) { a.start = time.Time{} },
			message: "timed by its caller",
		},
		{
			name:    "rejects a zero finished time",
			spoil:   func(a *attemptArgs) { a.end = time.Time{} },
			message: "timed by its caller",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := valid()
			tt.spoil(&args)

			_, err := NewAttempt(args.id, args.request, args.position, args.retry, args.backend, args.model, args.outcome, args.class, args.start, args.end)
			if err == nil {
				t.Fatal("NewAttempt() succeeded, want the unusable call refused")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("NewAttempt() error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && !strings.Contains(err.Error(), tt.message) {
				t.Errorf("NewAttempt() error = %q, want it to contain %q", err, tt.message)
			}
		})
	}
}

// providerErrorOfOctets builds a json object of exactly the requested size:
// an error envelope with a body padded to length. The lengths are ASCII, so
// len(string) is the octet count and the arithmetic is exact.
func providerErrorOfOctets(octets int) string {
	const prefix = `{"error":{"body":"`
	const suffix = `"}}`
	body := octets - len(prefix) - len(suffix)
	if body < 0 {
		panic("providerErrorOfOctets: cannot build an object smaller than the envelope")
	}
	return prefix + strings.Repeat("x", body) + suffix
}

// TestSetProviderErrorStoresAnObjectCopyWithinTheCap pins the one sanctioned
// jsonb's rules: an object by contract (arrays, strings, numbers and invalid
// json are all refused), capped at 32768 octets — half the database's own
// guard, with the cap exactly on the boundary — stored as a copy so no
// caller can rewrite telemetry it already handed over, replaceable in full
// by a later valid set, and clearable with an empty value. A refusal must
// leave previously stored telemetry standing: a refused attachment is not a
// licence to wipe what is already there.
func TestSetProviderErrorStoresAnObjectCopyWithinTheCap(t *testing.T) {
	t.Run("accepts an object at exactly the 32768-octet cap", func(t *testing.T) {
		attempt := formedAttempt(t)
		telemetry := providerErrorOfOctets(32768)
		if err := attempt.SetProviderError(json.RawMessage(telemetry)); err != nil {
			t.Fatalf("SetProviderError() at the cap error = %v", err)
		}
		if string(attempt.ProviderError) != telemetry {
			t.Error("SetProviderError() stored a value different from the one handed over")
		}
	})

	t.Run("refuses shapes that are not objects without touching stored telemetry", func(t *testing.T) {
		attempt := formedAttempt(t)
		original := json.RawMessage(`{"error":{"code":429}}`)
		if err := attempt.SetProviderError(original); err != nil {
			t.Fatalf("seeding valid telemetry: %v", err)
		}

		for _, rejected := range []string{`[1,2]`, `"str"`, `123`, `true`, `null`, `{"broken":`} {
			err := attempt.SetProviderError(json.RawMessage(rejected))
			if err == nil {
				t.Errorf("SetProviderError(%s) succeeded, want the non-object refused", rejected)
				continue
			}
			if !strings.Contains(err.Error(), "json object") {
				t.Errorf("SetProviderError(%s) error = %q, want it to name the object requirement", rejected, err)
			}
			if string(attempt.ProviderError) != string(original) {
				t.Fatalf("stored telemetry changed to %s after refusing %s", attempt.ProviderError, rejected)
			}
		}
	})

	t.Run("refuses one octet over the cap", func(t *testing.T) {
		attempt := formedAttempt(t)
		err := attempt.SetProviderError(json.RawMessage(providerErrorOfOctets(32769)))
		if err == nil {
			t.Fatal("SetProviderError() over the cap succeeded, want a refusal")
		}
		if !strings.Contains(err.Error(), "32768") {
			t.Errorf("SetProviderError() error = %q, want it to name the cap", err)
		}
		if attempt.ProviderError != nil {
			t.Errorf("stored telemetry = %s, want it untouched by the refused set", attempt.ProviderError)
		}
	})

	t.Run("stores a copy the caller cannot rewrite", func(t *testing.T) {
		attempt := formedAttempt(t)
		telemetry := []byte(`{"error":{"code":"stream_broken"}}`)
		if err := attempt.SetProviderError(telemetry); err != nil {
			t.Fatalf("SetProviderError() error = %v", err)
		}
		for i := range telemetry {
			telemetry[i] = 'x'
		}
		if string(attempt.ProviderError) != `{"error":{"code":"stream_broken"}}` {
			t.Errorf("stored telemetry = %s, want the value as handed over — the caller mutated its own slice, not the attempt's", attempt.ProviderError)
		}
	})

	t.Run("replaces a previous telemetry whole", func(t *testing.T) {
		attempt := formedAttempt(t)
		if err := attempt.SetProviderError(json.RawMessage(`{"error":{"attempt":1}}`)); err != nil {
			t.Fatalf("first SetProviderError() error = %v", err)
		}
		second := `{"error":{"attempt":2}}`
		if err := attempt.SetProviderError(json.RawMessage(second)); err != nil {
			t.Fatalf("second SetProviderError() error = %v", err)
		}
		if string(attempt.ProviderError) != second {
			t.Errorf("stored telemetry = %s, want the second set to have replaced the first", attempt.ProviderError)
		}
	})

	t.Run("an empty value clears to nil", func(t *testing.T) {
		attempt := formedAttempt(t)
		if err := attempt.SetProviderError(json.RawMessage(`{"error":{}}`)); err != nil {
			t.Fatalf("seeding telemetry: %v", err)
		}
		for name, empty := range map[string]json.RawMessage{"nil": nil, "zero length": {}} {
			if err := attempt.SetProviderError(empty); err != nil {
				t.Errorf("SetProviderError(%s) error = %v, want an empty value accepted as a clear", name, err)
			}
			if attempt.ProviderError != nil {
				t.Errorf("ProviderError = %s after the %s clear, want nil", attempt.ProviderError, name)
			}
		}
	})
}

// formedAttempt builds the fixture every SetProviderError case starts from:
// a post-commitment failure with the stream class, the one outcome where
// telemetry is expected and every cap and copy rule bites.
func formedAttempt(t *testing.T) *Attempt {
	t.Helper()
	attempt, err := NewAttempt("att-0001", "req-0001", 0, 1, "backend-anthropic", "claude-sonnet-4-5", OutcomeFailedAfterCommitment, ErrorStreamAfterCommitment, attemptStart, attemptEnd)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	return &attempt
}

// TestRecordProviderUsageFillsOnlyWhatNobodyClaimedYet pins the one
// sanctioned update: nil fields are filled, set fields are never overwritten
// by a later, different report (a second claim is a settlement decision, not
// a telemetry one), nil pointers leave their field untouched so a partial
// post-disconnect report can arrive in pieces, and a negative figure is
// refused whole — no field of a report carrying one is written, because
// usage is a count and there is no negative token.
func TestRecordProviderUsageFillsOnlyWhatNobodyClaimedYet(t *testing.T) {
	t.Run("writes the whole report onto nil fields", func(t *testing.T) {
		attempt := formedAttempt(t)
		input, output, delivery := int64(511), int64(137), int64(12)
		if err := attempt.RecordProviderUsage(&input, &output, &delivery); err != nil {
			t.Fatalf("RecordProviderUsage() error = %v", err)
		}
		if attempt.ProviderInputTokens == nil || attempt.ProviderOutputTokens == nil || attempt.DeliveryTokens == nil {
			t.Fatalf("usage = (%v, %v, %v), want all three written", attempt.ProviderInputTokens, attempt.ProviderOutputTokens, attempt.DeliveryTokens)
		}
		if *attempt.ProviderInputTokens != 511 || *attempt.ProviderOutputTokens != 137 || *attempt.DeliveryTokens != 12 {
			t.Errorf("usage = (%d, %d, %d), want (511, 137, 12)", *attempt.ProviderInputTokens, *attempt.ProviderOutputTokens, *attempt.DeliveryTokens)
		}
	})

	t.Run("refuses a negative figure wherever it sits and writes nothing", func(t *testing.T) {
		for name, report := range map[string][3]*int64{
			"negative input":    {ptrTo(int64(-1)), ptrTo(int64(10)), ptrTo(int64(10))},
			"negative output":   {ptrTo(int64(10)), ptrTo(int64(-1)), ptrTo(int64(10))},
			"negative delivery": {ptrTo(int64(10)), ptrTo(int64(10)), ptrTo(int64(-1))},
		} {
			attempt := formedAttempt(t)
			err := attempt.RecordProviderUsage(report[0], report[1], report[2])
			if !errors.Is(err, ErrUsageNegative) {
				t.Errorf("%s: RecordProviderUsage() = %v, want ErrUsageNegative", name, err)
			}
			if attempt.ProviderInputTokens != nil || attempt.ProviderOutputTokens != nil || attempt.DeliveryTokens != nil {
				t.Errorf("%s: usage = (%v, %v, %v), want all three untouched — a refused report writes nothing", name, attempt.ProviderInputTokens, attempt.ProviderOutputTokens, attempt.DeliveryTokens)
			}
		}
	})

	t.Run("zero is a claim and is written", func(t *testing.T) {
		attempt := formedAttempt(t)
		zero := int64(0)
		if err := attempt.RecordProviderUsage(&zero, &zero, &zero); err != nil {
			t.Fatalf("RecordProviderUsage() error = %v", err)
		}
		if attempt.ProviderInputTokens == nil || *attempt.ProviderInputTokens != 0 {
			t.Errorf("ProviderInputTokens = %v, want a non-nil zero — the provider said nothing was used, which is a claim nil could not carry", attempt.ProviderInputTokens)
		}
	})

	t.Run("a second report never overwrites the first claim", func(t *testing.T) {
		attempt := formedAttempt(t)
		first := []int64{511, 137, 12}
		if err := attempt.RecordProviderUsage(&first[0], &first[1], &first[2]); err != nil {
			t.Fatalf("first RecordProviderUsage() error = %v", err)
		}
		second := []int64{999, 999, 999}
		if err := attempt.RecordProviderUsage(&second[0], &second[1], &second[2]); err != nil {
			t.Fatalf("second RecordProviderUsage() error = %v", err)
		}
		if *attempt.ProviderInputTokens != 511 || *attempt.ProviderOutputTokens != 137 || *attempt.DeliveryTokens != 12 {
			t.Errorf("usage = (%d, %d, %d), want the first report standing — overwriting observed telemetry is a settlement decision, not this method's", *attempt.ProviderInputTokens, *attempt.ProviderOutputTokens, *attempt.DeliveryTokens)
		}
	})

	t.Run("nil pointers leave their field untouched", func(t *testing.T) {
		attempt := formedAttempt(t)
		input := int64(511)
		if err := attempt.RecordProviderUsage(&input, nil, nil); err != nil {
			t.Fatalf("first RecordProviderUsage() error = %v", err)
		}
		output, delivery := int64(137), int64(12)
		if err := attempt.RecordProviderUsage(nil, &output, &delivery); err != nil {
			t.Fatalf("second RecordProviderUsage() error = %v", err)
		}
		if *attempt.ProviderInputTokens != 511 || *attempt.ProviderOutputTokens != 137 || *attempt.DeliveryTokens != 12 {
			t.Errorf("usage = (%d, %d, %d), want the input from the first report and the rest from the second — a post-disconnect report arrives in pieces", *attempt.ProviderInputTokens, *attempt.ProviderOutputTokens, *attempt.DeliveryTokens)
		}
	})
}

// ptrTo exists because a table of *int64 literals needs a one-line way to
// take an address; it asserts nothing.
func ptrTo(value int64) *int64 {
	return &value
}
