//go:build integration

package postgres

// The settlement benchmarks: what the close — the unit of work every served
// request ends inside — costs against a real PostgreSQL. They exist for a
// developer holding the questions B11's close leaves open: how long the
// transaction is held, how many round trips the ending pays for, what a wide
// waterfall adds to the payload, and whether concurrent settlements contend
// on the stream. They are not a gate.
//
// The unit benchmarked is the one route_endings.go's settleOnce drives, in
// the same statement order and for the same reason: the CAS close first (the
// claim to the ending), the attempt probe and insert (the committed attempt
// joins its ending), the request and replay-record finalisations, and the
// fact append LAST — the one order in which a crash between the steps can
// strand neither a fact nor a hold.
//
// Run (from apps/dataplane):
//
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test -run '^$' -bench 'SettlementUnit' -benchmem -tags=integration ./internal/adapters/outbound/postgres

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// settleUnit is one prepared ending: everything the close unit touches,
// written with the timer stopped, so the timed loop holds nothing but the
// close itself.
type settleUnit struct {
	requestID identity.RequestID
	account   string
	key       string
	attempt   execution.Attempt
	reserveID identity.ReservationID
	legs      []accounting.AllocationLeg
}

// benchmarkSettleUnit prepares one unit: the request row, the replay record
// still open on its key, and the hold with its waterfall, all committed
// outside any measured section. The attempt deliberately does not exist yet —
// appending it is the unit's own work, the same way a committed attempt joins
// its ending in production.
func benchmarkSettleUnit(b *testing.B, ctx context.Context, repos integrationRepositories, account, key string, legs []accounting.AllocationLeg, hold int64) settleUnit {
	b.Helper()
	price := integrationPrice()
	now := time.Now().UTC()

	request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 120, 4096, price, now)
	if err != nil {
		b.Fatalf("forming a request: %v", err)
	}
	attempt, err := execution.NewAttempt(identity.NewAttemptID(), request.ID, 0, 0, "b7it-backend", "b7it/model", execution.OutcomeSucceeded, "", now, now)
	if err != nil {
		b.Fatalf("forming an attempt: %v", err)
	}
	record, err := execution.NewIntake(account, key, "b11-bench-digest", request.ID, now)
	if err != nil {
		b.Fatalf("forming the replay record: %v", err)
	}
	allocations := make([]accounting.Allocation, 0, len(legs))
	for i, leg := range legs {
		allocations = append(allocations, accounting.Allocation{
			FundingBucketID: leg.FundingBucketID,
			Amount:          leg.Amount,
			Ordinal:         i + 1,
		})
	}
	reservation, err := accounting.NewReservation(identity.NewReservationID(), request.ID, price.RevisionID,
		price.InputUnitPrice, price.OutputUnitPrice,
		request.InputTokens, request.MaxOutputTokens,
		hold, allocations, now, now.Add(time.Hour), "b11-bench", now.Add(30*time.Minute))
	if err != nil {
		b.Fatalf("forming a hold: %v", err)
	}
	if err := repos.store.WithinTx(ctx, func(ctx context.Context) error {
		if err := repos.requests.Insert(ctx, request); err != nil {
			return err
		}
		if err := repos.intakes.Insert(ctx, record); err != nil {
			return err
		}
		return repos.reserves.Insert(ctx, reservation)
	}); err != nil {
		b.Fatalf("writing the settled unit's setup rows: %v", err)
	}
	return settleUnit{
		requestID: request.ID,
		account:   account,
		key:       key,
		attempt:   attempt,
		reserveID: reservation.ID,
		legs:      legs,
	}
}

// runSettlementUnit is the measured section the benchmarks below share —
// settleOnce's statement sequence, nothing else inside the transaction.
func runSettlementUnit(b *testing.B, ctx context.Context, repos integrationRepositories, unit settleUnit) {
	b.Helper()
	price := integrationPrice()
	err := repos.store.WithinTx(ctx, func(ctx context.Context) error {
		now := time.Now().UTC()
		closed, err := repos.reserves.Close(ctx, unit.reserveID, accounting.StateSettled, now)
		if err != nil {
			return err
		}
		if !closed {
			return errors.New("reservation.Close() = false inside the unit that opened it")
		}
		appended, err := repos.attempts.Exists(ctx, unit.attempt.ID)
		if err != nil {
			return err
		}
		if !appended {
			if err := repos.attempts.Insert(ctx, unit.attempt); err != nil {
				return err
			}
		}
		finale := execution.Request{ID: unit.requestID, Status: execution.StatusExecuting}
		if err := finale.Succeed(unit.attempt.ID, now); err != nil {
			return err
		}
		finalised, err := repos.requests.Finalise(ctx, finale)
		if err != nil {
			return err
		}
		if !finalised {
			return errors.New("the request row did not finalise")
		}
		decided, err := repos.intakes.Finalise(ctx, unit.account, unit.key, execution.FinalSucceeded, "", "")
		if err != nil {
			return err
		}
		if !decided {
			return errors.New("the replay record did not finalise")
		}
		fact, err := accounting.NewSettled(unit.requestID, unit.attempt.ID, accounting.CaptureReported,
			int64Ptr(120), int64Ptr(45), int64Ptr(45),
			price.RevisionID, price.InputUnitPrice, price.OutputUnitPrice, integrationSettledAmount(),
			unit.legs, now)
		if err != nil {
			return err
		}
		_, err = repos.facts.Append(ctx, fact)
		return err
	})
	if err != nil {
		b.Fatalf("settlement unit: %v", err)
	}
}

// BenchmarkSettlementUnit costs one close end to end: five statements inside
// one transaction plus the commit — the latency every served request adds to
// its own ending, held against a real store.
func BenchmarkSettlementUnit(b *testing.B) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-settle-unit")
	units := make([]settleUnit, b.N)
	b.StopTimer()
	for i := range units {
		units[i] = benchmarkSettleUnit(b, ctx, repos, account,
			fmt.Sprintf("%s-unit-key-%d", account, i),
			[]accounting.AllocationLeg{{FundingBucketID: account + "-bench-bucket", Amount: 250, Ordinal: 1}},
			250)
	}
	b.StartTimer()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range units {
		runSettlementUnit(b, ctx, repos, units[i])
	}
}

// BenchmarkContendedSettlementUnit is the same close under the production
// load shape: many requests ending at once. Every parallel iteration closes
// its own reservation — the CAS never contends — while the fact appends share
// one stream, so the number shows what concurrent endings pay for the
// sequence the feed is.
func BenchmarkContendedSettlementUnit(b *testing.B) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-settle-contended")
	units := make([]settleUnit, b.N)
	b.StopTimer()
	for i := range units {
		units[i] = benchmarkSettleUnit(b, ctx, repos, account,
			fmt.Sprintf("%s-contended-key-%d", account, i),
			[]accounting.AllocationLeg{{FundingBucketID: account + "-bench-bucket", Amount: 250, Ordinal: 1}},
			250)
	}
	b.StartTimer()

	var next int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := int(atomic.AddInt64(&next, 1)) - 1
			runSettlementUnit(b, ctx, repos, units[i])
		}
	})
}

// BenchmarkSettlementUnitWideWaterfall is the payload-size dimension: the
// same close with its hold split across thirty-two buckets, so the fact's
// allocation tail — and the JSON the append carries — is thirty-two legs
// instead of one. The delta against BenchmarkSettlementUnit is what width
// costs the ending.
func BenchmarkSettlementUnitWideWaterfall(b *testing.B) {
	const width = 32
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-settle-wide")
	legs := make([]accounting.AllocationLeg, 0, width)
	for i := 0; i < width; i++ {
		legs = append(legs, accounting.AllocationLeg{
			FundingBucketID: fmt.Sprintf("%s-wide-bucket-%02d", account, i),
			Amount:          10,
			Ordinal:         i + 1,
		})
	}
	units := make([]settleUnit, b.N)
	b.StopTimer()
	for i := range units {
		units[i] = benchmarkSettleUnit(b, ctx, repos, account,
			fmt.Sprintf("%s-wide-key-%d", account, i), legs, 10*width)
	}
	b.StartTimer()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range units {
		runSettlementUnit(b, ctx, repos, units[i])
	}
}
