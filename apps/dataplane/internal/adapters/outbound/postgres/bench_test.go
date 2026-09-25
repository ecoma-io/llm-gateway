//go:build integration

package postgres

// The runtime-storage benchmarks: what the B7 write paths and the fact
// reader cost against a real PostgreSQL, for a developer holding a decision
// about pool sizing, payload shape or page limits — not a gate. They share
// the suite's helpers (runtime_storage_test.go), the same fixture database
// and its unique-per-run accounts, so they can run beside the tests and
// against a database that already carries rows.
//
// Run (from apps/dataplane):
//
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test -run '^$' -bench . -benchmem -tags=integration ./internal/adapters/outbound/postgres
//
// A short -benchtime (10x) is enough to see the shape; the numbers that
// matter are ns/op and B/op against the previous run, not the absolute
// values, which belong to whatever machine and database ran them.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// benchmarkContext bounds one benchmark's whole run. testing.B has no
// context of its own, and the database work these benchmarks drive is
// wait-prone in ways a hung run should not be — five minutes per benchmark
// is far more than any of them needs and short enough that a wedged storage
// ends the run instead of the developer's patience.
func benchmarkContext(b *testing.B) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	b.Cleanup(cancel)
	return ctx
}

// BenchmarkRequestInsert costs the one write every request path starts with:
// a single-row insert into public.requests, its fifteen columns and its
// checks. The request is formed outside the timer — forming is memory work
// this benchmark is not about — and every iteration inserts a distinct row,
// because the interesting cost is the insert's, not the unique index's
// conflict path.
func BenchmarkRequestInsert(b *testing.B) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-request")
	requests := make([]execution.Request, b.N)
	for i := range requests {
		request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 120, 4096, integrationPrice(), time.Now().UTC())
		if err != nil {
			b.Fatalf("forming a request: %v", err)
		}
		requests[i] = request
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := repos.requests.Insert(ctx, requests[i]); err != nil {
			b.Fatalf("inserting request %d: %v", i, err)
		}
	}
}

// BenchmarkAdmissionUnit costs the admission unit of work as the ports'
// doctrine draws it: the replay record insert, the request insert, the
// waterfall drawdown and the hold insert, one WithinTx, the drawdown's legs
// feeding the reservation. One grant seeded once, deep enough that capacity
// is never the variable; the iteration count is the number of admissions,
// not the number of statements.
func BenchmarkAdmissionUnit(b *testing.B) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-admission")
	integrationSeedProjection(b, ctx, repos, account, true, time.Now().UTC().Add(24*time.Hour), 1_000_000_000_000_000)
	scope := repos.catalogScope(b)
	price := integrationPrice()

	// Formed up front: the identity minting and struct building are not the
	// unit's cost, the round trips and the waterfall are.
	intakes := make([]execution.Intake, b.N)
	requests := make([]execution.Request, b.N)
	for i := 0; i < b.N; i++ {
		request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 120, 4096, price, time.Now().UTC())
		if err != nil {
			b.Fatalf("forming a request: %v", err)
		}
		requests[i] = request
		intake, err := execution.NewIntake(account, fmt.Sprintf("bench-key-%s-%d", account, i), "bench-digest", request.ID, time.Now().UTC())
		if err != nil {
			b.Fatalf("forming an intake: %v", err)
		}
		intakes[i] = intake
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := store.WithinTx(ctx, func(ctx context.Context) error {
			if err := repos.intakes.Insert(ctx, intakes[i]); err != nil {
				return err
			}
			if err := repos.requests.Insert(ctx, requests[i]); err != nil {
				return err
			}
			drawn, err := repos.quota.Drawdown(ctx, account, scope.alias, 250)
			if err != nil {
				return err
			}
			reservation, err := accounting.NewReservation(
				identity.NewReservationID(), requests[i].ID, price.RevisionID,
				price.InputUnitPrice, price.OutputUnitPrice,
				requests[i].InputTokens, requests[i].MaxOutputTokens,
				250, drawn, time.Now().UTC(), time.Now().UTC().Add(time.Hour), "b7it-runtime", time.Now().UTC().Add(30*time.Minute),
			)
			if err != nil {
				return err
			}
			return repos.reserves.Insert(ctx, reservation)
		})
		if err != nil {
			b.Fatalf("admission unit %d: %v", i, err)
		}
	}
}

// BenchmarkContendedDrawdown costs the conditional drawdown under real
// contention: every parallel unit takes 40 minor units from the same single
// bucket, so the row lock is the benchmark, and the pool's four connections
// queue behind it. The grant is seeded with twice the capacity b.N draws
// could consume — headroom, so a refusal means the contention path decided
// it and not the arithmetic; refusals are the walk's honest answer under a
// stale snapshot and are counted, not failed. What must never happen is what
// the post-check looks for: a balance below zero, the one outcome the
// conditional update exists to make impossible.
func BenchmarkContendedDrawdown(b *testing.B) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-contended")
	const draw = int64(40)
	integrationSeedProjection(b, ctx, repos, account, true, time.Now().UTC().Add(24*time.Hour), 2*int64(b.N)*draw)
	scope := repos.catalogScope(b)

	var (
		mu           sync.Mutex
		insufficient int
		firstRefused error
		firstFailure error
	)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := store.WithinTx(ctx, func(ctx context.Context) error {
				_, err := repos.quota.Drawdown(ctx, account, scope.alias, draw)
				return err
			})
			switch {
			case err == nil:
			case errors.Is(err, accounting.ErrInsufficientCapacity):
				mu.Lock()
				if firstRefused == nil {
					firstRefused = err
				}
				insufficient++
				mu.Unlock()
			default:
				mu.Lock()
				if firstFailure == nil {
					firstFailure = err
				}
				mu.Unlock()
			}
		}
	})
	b.StopTimer()

	if firstFailure != nil {
		b.Fatalf("a contended drawdown failed: %v", firstFailure)
	}
	if insufficient > 0 {
		b.Logf("%d of %d contended draws were refused by the walk (stale snapshots under the row lock)", insufficient, b.N)
	}
	if available, _, _, _ := integrationProjectionRow(b, db, account+"-bucket"); available < 0 {
		b.Fatalf("available after the contention = %d, want a floor of zero — a negative balance is the outcome this design refuses", available)
	}
}

// BenchmarkAttemptInsert costs the one append-only write a completion makes.
// One request, b.N attempts beneath it, each with its own retry sequence —
// the row's identity within the request — formed outside the timer.
func BenchmarkAttemptInsert(b *testing.B) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-attempt")
	request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 120, 4096, integrationPrice(), time.Now().UTC())
	if err != nil {
		b.Fatalf("forming the request: %v", err)
	}
	if err := repos.requests.Insert(ctx, request); err != nil {
		b.Fatalf("inserting the request: %v", err)
	}

	attempts := make([]execution.Attempt, b.N)
	for i := range attempts {
		attempt, err := execution.NewAttempt(identity.NewAttemptID(), request.ID, 0, i, "b7it-backend", "b7it/model", execution.OutcomeSucceeded, "", time.Now().UTC(), time.Now().UTC())
		if err != nil {
			b.Fatalf("forming attempt %d: %v", i, err)
		}
		attempts[i] = attempt
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := repos.attempts.Insert(ctx, attempts[i]); err != nil {
			b.Fatalf("inserting attempt %d: %v", i, err)
		}
	}
}

// BenchmarkReservationCloseAndFactAppend costs the settlement's tail: the
// once-only close and the fact append that must be its last statement, one
// unit of work. The setup — request, attempt, hold — runs with the timer
// stopped, because the tail is what a settlement's critical section is made
// of; the stream row lock it holds to commit is the number this benchmark
// exists to show.
func BenchmarkReservationCloseAndFactAppend(b *testing.B) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-settle")
	price := integrationPrice()

	type prepared struct {
		reservationID identity.ReservationID
		fact          accounting.Fact
	}
	units := make([]prepared, b.N)
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 120, 4096, price, time.Now().UTC())
		if err != nil {
			b.Fatalf("forming a request: %v", err)
		}
		attempt, err := execution.NewAttempt(identity.NewAttemptID(), request.ID, 0, 0, "b7it-backend", "b7it/model", execution.OutcomeSucceeded, "", time.Now().UTC(), time.Now().UTC())
		if err != nil {
			b.Fatalf("forming an attempt: %v", err)
		}
		reservation, err := accounting.NewReservation(identity.NewReservationID(), request.ID, price.RevisionID,
			price.InputUnitPrice, price.OutputUnitPrice,
			request.InputTokens, request.MaxOutputTokens,
			0, nil, time.Now().UTC(), time.Now().UTC().Add(time.Hour), "b7it-runtime", time.Now().UTC().Add(30*time.Minute))
		if err != nil {
			b.Fatalf("forming a hold: %v", err)
		}
		// The fact is formed here too, with the timer stopped: its building is
		// the domain's struct work, not the unit's round trips, and the timed
		// loop should hold nothing but the close and the append it exists to
		// measure.
		fact, err := accounting.NewSettled(request.ID, attempt.ID, accounting.CaptureReported,
			int64Ptr(120), int64Ptr(45), int64Ptr(45),
			price.RevisionID, price.InputUnitPrice, price.OutputUnitPrice, 375,
			[]accounting.AllocationLeg{{FundingBucketID: account + "-bench-bucket", Amount: 250, Ordinal: 1}},
			time.Now().UTC())
		if err != nil {
			b.Fatalf("forming settlement fact %d: %v", i, err)
		}
		if err := store.WithinTx(ctx, func(ctx context.Context) error {
			if err := repos.requests.Insert(ctx, request); err != nil {
				return err
			}
			if err := repos.attempts.Insert(ctx, attempt); err != nil {
				return err
			}
			return repos.reserves.Insert(ctx, reservation)
		}); err != nil {
			b.Fatalf("inserting settlement unit %d: %v", i, err)
		}
		units[i] = prepared{reservationID: reservation.ID, fact: fact}
	}
	b.StartTimer()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := store.WithinTx(ctx, func(ctx context.Context) error {
			closed, err := repos.reserves.Close(ctx, units[i].reservationID, accounting.StateSettled, time.Now().UTC())
			if err != nil {
				return err
			}
			if !closed {
				return errors.New("reservation.Close() = false inside the unit that opened it")
			}
			_, err = repos.facts.Append(ctx, units[i].fact)
			return err
		})
		if err != nil {
			b.Fatalf("settlement tail %d: %v", i, err)
		}
	}
}

// benchmarkFactPageRead is the reader's cost at one page size: the stream
// query, the page query and limit+1 rows of payload decode, timed against a
// feed section this benchmark appended for itself — limit+10 facts past a
// cursor taken before any of them, so the page is full, has more behind it,
// and belongs to no other run. Every iteration reads the same page; the
// page's cost, not the feed's growth, is the measurement.
func benchmarkFactPageRead(b *testing.B, limit int) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	reader := NewUsageFacts(db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, fmt.Sprintf("bench-read-%d", limit))
	start := integrationStreamCursor(b, db)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < limit+10; i++ {
		if _, _, seq := integrationAppendSettled(b, ctx, repos, account,
			[]accounting.AllocationLeg{{FundingBucketID: account + "-bench-bucket", Amount: 250, Ordinal: 1}},
			base.Add(time.Duration(i)*time.Millisecond)); seq == 0 {
			b.Fatalf("append %d allocated no sequence", i)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := reader.Read(ctx, start, limit)
		if err != nil {
			b.Fatalf("reading a %d-event page: %v", limit, err)
		}
		if len(page.Events) != limit || !page.HasMore {
			b.Fatalf("the page carried %d events (HasMore %t), want %d and true — the benchmark's own feed is wrong", len(page.Events), page.HasMore, limit)
		}
	}
}

// BenchmarkFactPageRead100 is the feed page the port's default serves.
func BenchmarkFactPageRead100(b *testing.B) { benchmarkFactPageRead(b, 100) }

// BenchmarkFactPageRead1000 is the deep page: ten times the rows and the
// payload bytes, where the per-row decode cost shows.
func BenchmarkFactPageRead1000(b *testing.B) { benchmarkFactPageRead(b, 1000) }

// BenchmarkIntakeLookup costs the replay decision's read: one row, by its
// two-column key, the query every replayed request waits on. The record is
// written once before the timer; the lookup is the whole measurement.
func BenchmarkIntakeLookup(b *testing.B) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-intake")
	requestID := identity.NewRequestID()
	intake, err := execution.NewIntake(account, "bench-lookup-key", "bench-digest", requestID, time.Now().UTC())
	if err != nil {
		b.Fatalf("forming the replay record: %v", err)
	}
	if err := repos.intakes.Insert(ctx, intake); err != nil {
		b.Fatalf("inserting the replay record: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := repos.intakes.Find(ctx, account, "bench-lookup-key"); err != nil {
			b.Fatalf("looking up the replay record: %v", err)
		}
	}
}
