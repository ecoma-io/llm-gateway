package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
)

// The projection delivery loop's tests. One Reconcile call is one cycle, and
// every cycle is the same judgement made twice — once at the top, where the
// position either joins the log's timeline or is answered with a snapshot,
// and once inside the drain, where a refusal either means "deliver
// differently" (heal in the same cycle) or "this build is wrong" (fail
// loudly). The fakes below script both halves declaratively — pages on the
// log, answers on the mirror — and share one trace so a test can see the
// order the ports were touched in, which is where a cycle's story is legible:
// whether a snapshot preceded the first batch, whether a heal re-read
// nothing, whether a refused delivery was the last thing that happened. What
// they deliberately do not pin is HTTP; that is the adapter tier's job.

// The errors a scripted cycle can plant in either side's reads. They are
// plain sentinels so the tests can prove the loop's errors wrap their cause
// with errors.Is rather than merely mentioning it.
var (
	errFakeHead     = errors.New("fake: the head read failed")
	errFakePosition = errors.New("fake: the position read failed")
	errFakeSnapshot = errors.New("fake: the snapshot cut failed")
	errFakeChanges  = errors.New("fake: the log read failed")
)

// projectedAt is the recorded instant carried by every change the tests mint.
// It orders nothing (ADR 0007 §3); it exists so the changes are legal.
var projectedAt = time.Date(2026, 9, 25, 8, 9, 10, 0, time.UTC)

// timeline names a producer epoch in the canonical UUIDv4 form the grammar
// demands: version nibble 4, RFC 4122 variant, lowercase hex throughout. Seq
// merely keeps distinct timelines distinct.
func timeline(seq int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", seq)
}

// logChange mints one legal log entry at rev — a full-state credential row,
// as every entry of the durable log is.
func logChange(t *testing.T, rev uint64) projection.Change {
	t.Helper()
	keyID := fmt.Sprintf("a0000000-0000-4000-8000-%012x", rev)
	accountID := fmt.Sprintf("b0000000-0000-4000-8000-%012x", rev)
	credential, err := projection.NewCredential(keyID, accountID, fmt.Sprintf("%064x", rev), projection.CredentialActive, nil)
	if err != nil {
		t.Fatalf("the test minted an illegal credential: %v", err)
	}
	change, err := projection.NewCredentialChange(rev, projectedAt, credential)
	if err != nil {
		t.Fatalf("the test minted an illegal change at revision %d: %v", rev, err)
	}
	return change
}

// logChanges mints the contiguous run of count entries starting strictly
// after from — exactly the page a healthy, gapless log returns.
func logChanges(t *testing.T, from uint64, count int) []projection.Change {
	t.Helper()
	changes := make([]projection.Change, 0, count)
	for i := 0; i < count; i++ {
		changes = append(changes, logChange(t, from+uint64(i)+1))
	}
	return changes
}

// scriptedLog is the fake ProjectionLog. A test scripts the head it reads,
// the revision its snapshot is cut at, and the sequence of pages ChangesAfter
// serves; every call is recorded — the revisions it was asked for most of
// all, because the resume point is the loop's most fragile number.
type scriptedLog struct {
	trace *[]string

	head        projection.Head
	headErr     error
	snapshotAt  uint64
	snapshotErr error
	pages       [][]projection.Change
	changesErr  error

	// switchHead, when set, is what Head returns from the
	// (switchAfter+1)-th call onward — the timeline re-minted under the
	// loop's feet, the restore procedure's operator step.
	switchAfter uint64
	switchHead  *projection.Head

	heads     int
	snapshots int
	afters    []uint64
	limits    []int
}

func (l *scriptedLog) Head(context.Context) (projection.Head, error) {
	l.heads++
	*l.trace = append(*l.trace, "head")
	if l.headErr != nil {
		return projection.Head{}, l.headErr
	}
	if l.switchHead != nil && uint64(l.heads) > l.switchAfter {
		return *l.switchHead, nil
	}
	return l.head, nil
}

func (l *scriptedLog) Snapshot(context.Context) (projection.Snapshot, error) {
	l.snapshots++
	*l.trace = append(*l.trace, "snapshot")
	if l.snapshotErr != nil {
		return projection.Snapshot{}, l.snapshotErr
	}
	cut, err := projection.NewSnapshot(l.head.Epoch, l.snapshotAt, nil, nil)
	if err != nil {
		panic("test: the scripted snapshot cut is not a legal value: " + err.Error())
	}
	return cut, nil
}

func (l *scriptedLog) ChangesAfter(_ context.Context, after uint64, limit int) ([]projection.Change, error) {
	*l.trace = append(*l.trace, fmt.Sprintf("changes:%d", after))
	l.afters = append(l.afters, after)
	l.limits = append(l.limits, limit)
	if l.changesErr != nil {
		return nil, l.changesErr
	}
	if len(l.pages) == 0 {
		// An exhausted script is a drained feed: fewer than limit entries, no
		// more writers. The loop's answer to it is to stop.
		return nil, nil
	}
	page := l.pages[0]
	l.pages = l.pages[1:]
	return page, nil
}

func (l *scriptedLog) Credential(context.Context, string) (projection.Credential, error) {
	return projection.Credential{}, errors.New("fake: Credential is not part of the delivery path")
}

// snapshotScript and changeScript are one scripted answer each: the ack the
// mirror returns, or the refusal it raises instead. cancelAfter lets a test
// cancel the cycle's context from inside a successful delivery, which is how
// the drain's guard on the next page is reached deterministically.
type snapshotScript struct {
	ack dataplane.ProjectionAck
	err error
}

type changeScript struct {
	ack         dataplane.ProjectionAck
	err         error
	cancelAfter func()
}

// scriptedMirror is the fake Projection consumer. It judges every message it
// is handed — decoding the envelope the producer built, because a consumer
// that could not read the message could not accept or refuse it — and answers
// from its script. A call that outruns the script is a test defect, and it
// fails loudly rather than inventing an answer.
type scriptedMirror struct {
	trace *[]string

	position      dataplane.ProjectionPosition
	positionErr   error
	snapshotCalls []snapshotScript
	changeCalls   []changeScript

	positions         int
	snapshots         int
	deliveries        int
	deliveredEpochs   []string
	snapshotRevisions []uint64
	batchFroms        []uint64
	batchLasts        []uint64
}

func (m *scriptedMirror) ProjectionPosition(context.Context) (dataplane.ProjectionPosition, error) {
	m.positions++
	*m.trace = append(*m.trace, "position")
	if m.positionErr != nil {
		return dataplane.ProjectionPosition{}, m.positionErr
	}
	return m.position, nil
}

// judged is what the fake consumer read off one message: which timeline it
// names, and the revision window it covers.
type judged struct {
	epoch    string
	snapshot uint64
	from     uint64
	last     uint64
}

func (m *scriptedMirror) judge(message []byte) judged {
	var wire struct {
		Epoch            string            `json:"epoch"`
		SnapshotRevision uint64            `json:"snapshot_revision"`
		FromRevision     uint64            `json:"from_revision"`
		Changes          []json.RawMessage `json:"changes"`
	}
	if err := json.Unmarshal(message, &wire); err != nil {
		panic("test: the fake consumer could not read a message the producer built: " + err.Error())
	}
	if len(wire.Changes) == 0 {
		return judged{epoch: wire.Epoch, snapshot: wire.SnapshotRevision, last: wire.SnapshotRevision}
	}
	return judged{
		epoch: wire.Epoch,
		from:  wire.FromRevision,
		last:  wire.FromRevision + uint64(len(wire.Changes)),
	}
}

func (m *scriptedMirror) DeliverSnapshot(_ context.Context, message []byte) (dataplane.ProjectionAck, error) {
	m.snapshots++
	read := m.judge(message)
	m.deliveredEpochs = append(m.deliveredEpochs, read.epoch)
	m.snapshotRevisions = append(m.snapshotRevisions, read.snapshot)
	*m.trace = append(*m.trace, "deliver:snapshot")
	if len(m.snapshotCalls) == 0 {
		return dataplane.ProjectionAck{}, fmt.Errorf("fake: unexpected DeliverSnapshot call %d", m.snapshots)
	}
	call := m.snapshotCalls[0]
	m.snapshotCalls = m.snapshotCalls[1:]
	if call.err != nil {
		return dataplane.ProjectionAck{}, call.err
	}
	return call.ack, nil
}

func (m *scriptedMirror) DeliverChanges(_ context.Context, message []byte) (dataplane.ProjectionAck, error) {
	m.deliveries++
	read := m.judge(message)
	m.deliveredEpochs = append(m.deliveredEpochs, read.epoch)
	m.batchFroms = append(m.batchFroms, read.from)
	m.batchLasts = append(m.batchLasts, read.last)
	*m.trace = append(*m.trace, "deliver:changes")
	if len(m.changeCalls) == 0 {
		return dataplane.ProjectionAck{}, fmt.Errorf("fake: unexpected DeliverChanges call %d", m.deliveries)
	}
	call := m.changeCalls[0]
	m.changeCalls = m.changeCalls[1:]
	if call.err != nil {
		return dataplane.ProjectionAck{}, call.err
	}
	if call.cancelAfter != nil {
		call.cancelAfter()
	}
	return call.ack, nil
}

// deliveryHarness wires the producer to one scripted log and one scripted
// mirror over a shared trace, the way every test here builds the world.
type deliveryHarness struct {
	log      *scriptedLog
	mirror   *scriptedMirror
	trace    *[]string
	producer *ProjectionDelivery
}

func newDeliveryHarness(epoch string, headRevision uint64, position dataplane.ProjectionPosition) *deliveryHarness {
	trace := &[]string{}
	log := &scriptedLog{trace: trace, head: projection.Head{Epoch: epoch, LastRevision: headRevision}}
	mirror := &scriptedMirror{trace: trace, position: position}
	return &deliveryHarness{
		log:      log,
		mirror:   mirror,
		trace:    trace,
		producer: NewProjectionDelivery(log, mirror),
	}
}

func (h *deliveryHarness) run(t *testing.T) error {
	t.Helper()
	return h.producer.Reconcile(context.Background(), false)
}

// ack is shorthand for a scripted answer that accepts at rev.
func ack(rev uint64) snapshotScript {
	return snapshotScript{ack: dataplane.ProjectionAck{AppliedRevision: rev}}
}

// changeAck is the same shorthand for a batch delivery.
func changeAck(rev uint64) changeScript {
	return changeScript{ack: dataplane.ProjectionAck{AppliedRevision: rev}}
}

func TestFirstCycleBootstrapsAnUnbornMirrorAndDrainsFromItsAck(t *testing.T) {
	// A mirror that has never bootstrapped has no position to join with, so
	// the cycle's first act must be a whole snapshot and nothing incremental
	// may precede it. What the test really pins is the resume point: the drain
	// asks the log for what follows the acknowledgement the mirror committed —
	// revision 2, not the zero the producer guessed and not the snapshot cut's
	// own boundary — because the ack, not the producer's arithmetic, is where
	// the mirror's position lives.
	epoch := timeline(1)
	h := newDeliveryHarness(epoch, 10, dataplane.ProjectionPosition{ProtocolVersion: projection.ProtocolVersion})
	h.log.snapshotAt = 9
	h.log.pages = [][]projection.Change{logChanges(t, 2, 3), logChanges(t, 5, 2), nil}
	h.mirror.snapshotCalls = []snapshotScript{ack(2)}
	h.mirror.changeCalls = []changeScript{changeAck(5), changeAck(7)}

	if err := h.run(t); err != nil {
		t.Fatalf("Reconcile returned %v, want a clean first cycle", err)
	}
	if h.mirror.snapshots != 1 {
		t.Fatalf("got %d snapshots for an unborn mirror, want 1 — bootstrap is one whole delivery", h.mirror.snapshots)
	}
	if got, want := h.log.afters, []uint64{2, 5, 7}; !equalRevisions(got, want) {
		t.Fatalf("got ChangesAfter calls at %v, want %v — the drain resumes from the snapshot's ack, not from zero", got, want)
	}
	if got, want := h.mirror.batchFroms, []uint64{2, 5}; !equalRevisions(got, want) {
		t.Fatalf("got batches from %v, want %v — each page assembled exactly where the last ack left off", got, want)
	}
	if got, want := *h.trace, []string{
		"head", "position", "snapshot", "deliver:snapshot", "changes:2", "deliver:changes", "changes:5", "deliver:changes", "changes:7",
	}; !equalStrings(*h.trace, want) {
		t.Fatalf("got trace %v, want %v — no batch may precede the bootstrap snapshot", got, want)
	}
}

func TestAJoinedPositionDrainsStraightFromItsAppliedRevision(t *testing.T) {
	// The ordinary tick: a bootstrapped mirror on this timeline, at the log's
	// head, owes nothing but the entries after its applied revision. A
	// snapshot here would be a cycle that re-delivered the world for no
	// reason, and the test holds the line at zero of them.
	epoch := timeline(2)
	position := dataplane.ProjectionPosition{
		ProtocolVersion: projection.ProtocolVersion,
		Bootstrapped:    true,
		Epoch:           epoch,
		AppliedRevision: 7,
	}
	h := newDeliveryHarness(epoch, 10, position)
	h.log.pages = [][]projection.Change{logChanges(t, 7, 3), nil}
	h.mirror.changeCalls = []changeScript{changeAck(10)}

	if err := h.run(t); err != nil {
		t.Fatalf("Reconcile returned %v, want a clean drain", err)
	}
	if h.mirror.snapshots != 0 {
		t.Fatalf("got %d snapshots for a joined position, want 0 — a joined mirror is never re-bootstrapped", h.mirror.snapshots)
	}
	if got, want := h.log.afters, []uint64{7, 10}; !equalRevisions(got, want) {
		t.Fatalf("got ChangesAfter calls at %v, want %v — the drain starts at the applied revision, not at zero", got, want)
	}
	if h.mirror.deliveries != 1 {
		t.Fatalf("got %d batch deliveries, want 1 — one page, one batch", h.mirror.deliveries)
	}
}

func TestPositionsThatCannotJoinTheTimelineAreAnsweredWithASnapshot(t *testing.T) {
	// Three ways a position can fail to join a timeline, and the protocol has
	// one answer to all of them: a snapshot cut at the head, delivered whole,
	// with the drain resuming from the boundary the mirror committed. The
	// cases are deliberately different in kind — a foreign epoch, a position
	// that has run past the log's head because the producer's database was
	// restored, and a mirror that never bootstrapped at all — because a loop
	// that reasoned its way around any one of them would be guessing where a
	// snapshot refuses to.
	const (
		epoch       = "00000000-0000-4000-8000-000000000003"
		foreign     = "00000000-0000-4000-8000-000000000004"
		snapshotAck = 4
	)
	tests := []struct {
		name     string
		why      string
		position dataplane.ProjectionPosition
	}{
		{
			name: "a position earned on a foreign timeline",
			why:  "an epoch mismatch cannot be repaired by revisions that will never be re-issued",
			position: dataplane.ProjectionPosition{
				ProtocolVersion: projection.ProtocolVersion,
				Bootstrapped:    true,
				Epoch:           foreign,
				AppliedRevision: 7,
			},
		},
		{
			name: "a position that has run past the log's head",
			why:  "the restore case — the mirror is ahead of a timeline that was rewound under it",
			position: dataplane.ProjectionPosition{
				ProtocolVersion: projection.ProtocolVersion,
				Bootstrapped:    true,
				Epoch:           epoch,
				AppliedRevision: 15,
			},
		},
		{
			name: "a mirror that never bootstrapped",
			why:  "an incremental batch needs a position to join, and there is none",
			position: dataplane.ProjectionPosition{
				ProtocolVersion: projection.ProtocolVersion,
				AppliedRevision: 10,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newDeliveryHarness(epoch, 10, tt.position)
			h.log.snapshotAt = 9
			h.log.pages = [][]projection.Change{logChanges(t, snapshotAck, 2), nil}
			h.mirror.snapshotCalls = []snapshotScript{ack(snapshotAck)}
			h.mirror.changeCalls = []changeScript{changeAck(6)}

			if err := h.run(t); err != nil {
				t.Fatalf("Reconcile returned %v, want a snapshot to heal the position (%s)", err, tt.why)
			}
			if h.mirror.snapshots != 1 {
				t.Fatalf("got %d snapshots, want 1 — %s", h.mirror.snapshots, tt.why)
			}
			if got, want := h.log.afters, []uint64{snapshotAck, 6}; !equalRevisions(got, want) {
				t.Fatalf("got ChangesAfter calls at %v, want %v — the drain resumes from the snapshot's ack", got, want)
			}
			if got, want := h.mirror.batchFroms, []uint64{snapshotAck}; !equalRevisions(got, want) {
				t.Fatalf("got a batch from revision %d, want %d — the snapshot, not the stale position, names where delivery resumes", got, want)
			}
		})
	}
}

func TestAForcedCycleSnapshotsEvenAJoinedPosition(t *testing.T) {
	// force is the operator's answer to a mirror whose position says "fine"
	// while its rows say otherwise: the producer is being told to stop
	// trusting the join and re-establish the truth whole. The position here is
	// perfectly joined, and the snapshot must be delivered anyway — the flag
	// overrides the arithmetic, not decorates it.
	epoch := timeline(5)
	position := dataplane.ProjectionPosition{
		ProtocolVersion: projection.ProtocolVersion,
		Bootstrapped:    true,
		Epoch:           epoch,
		AppliedRevision: 10,
	}
	h := newDeliveryHarness(epoch, 10, position)
	h.log.snapshotAt = 10
	h.mirror.snapshotCalls = []snapshotScript{ack(10)}

	if err := h.producer.Reconcile(context.Background(), true); err != nil {
		t.Fatalf("forced Reconcile returned %v, want a forced snapshot cycle", err)
	}
	if h.mirror.snapshots != 1 {
		t.Fatalf("got %d snapshots under force, want 1 — force delivers the snapshot regardless of the join", h.mirror.snapshots)
	}
	if h.mirror.deliveries != 0 {
		t.Fatalf("got %d batch deliveries, want 0 — a joined position at the head owes no increments after the snapshot", h.mirror.deliveries)
	}
}

func TestTheDrainBatchesTheLogAtTheContractsPageSizeUntilTheFeedIsDry(t *testing.T) {
	// The drain's shape: full pages at MaxChangesPerBatch, then the short
	// page a concurrent writer produces, then the empty read that ends the
	// cycle. Each page becomes exactly one batch, contiguous from the previous
	// ack, and every batch travels under the epoch the cycle read at its
	// start — the timeline's name is read once and trusted for the whole run.
	const headRevision = 202 // 200 + 2: one full page, one short page
	epoch := timeline(6)
	position := dataplane.ProjectionPosition{
		ProtocolVersion: projection.ProtocolVersion,
		Bootstrapped:    true,
		Epoch:           epoch,
	}
	h := newDeliveryHarness(epoch, headRevision, position)
	h.log.pages = [][]projection.Change{
		logChanges(t, 0, projection.MaxChangesPerBatch),
		logChanges(t, projection.MaxChangesPerBatch, 2),
		nil,
	}
	h.mirror.changeCalls = []changeScript{
		changeAck(projection.MaxChangesPerBatch),
		changeAck(headRevision),
	}

	if err := h.run(t); err != nil {
		t.Fatalf("Reconcile returned %v, want a clean paged drain", err)
	}
	if got, want := h.mirror.deliveries, 2; got != want {
		t.Fatalf("got %d batch deliveries, want %d — one batch per page, no batching of batches", got, want)
	}
	if got, want := h.mirror.batchFroms, []uint64{0, projection.MaxChangesPerBatch}; !equalRevisions(got, want) {
		t.Fatalf("got batches from %v, want %v — each batch starts where the previous ack ended", got, want)
	}
	if got, want := h.mirror.batchLasts, []uint64{projection.MaxChangesPerBatch, uint64(headRevision)}; !equalRevisions(got, want) {
		t.Fatalf("got batches ending at %v, want %v — the pages carried %d entries then 2", got, want, projection.MaxChangesPerBatch)
	}
	if got, want := h.log.afters, []uint64{0, projection.MaxChangesPerBatch, uint64(headRevision)}; !equalRevisions(got, want) {
		t.Fatalf("got ChangesAfter calls at %v, want %v — the final empty read is what stops the loop", got, want)
	}
	for i, limit := range h.log.limits {
		if limit != projection.MaxChangesPerBatch {
			t.Fatalf("page %d asked for %d entries, want %d — the producer batches at the contract's bound, always", i, limit, projection.MaxChangesPerBatch)
		}
	}
	for i, batchEpoch := range h.mirror.deliveredEpochs {
		if batchEpoch != epoch {
			t.Fatalf("batch %d names epoch %q, want %q — the epoch read at cycle start travels on every batch", i, batchEpoch, epoch)
		}
	}
}

func TestAMidDrainRefusalHealsInTheSameCycleWithASnapshot(t *testing.T) {
	// The two refusals that mean "deliver differently" — a batch that does not
	// join the mirror's position, and a position from a timeline this producer
	// left — are answered inside the very cycle that raised them: a snapshot,
	// then the drain carries on from the boundary that snapshot committed. One
	// Reconcile call must contain all of it; the test watches the trace to
	// prove the heal re-read the head — the epoch the next batches must
	// speak, in case the timeline was re-minted under the loop — but not
	// the position, and that the loop's old `from` was replaced rather than
	// argued with.
	tests := []struct {
		name    string
		refusal error
	}{
		{
			name:    "the batch does not join the stored position",
			refusal: dataplane.ErrProjectionGap,
		},
		{
			name:    "the stored position cannot join this timeline",
			refusal: dataplane.ErrProjectionSnapshotRequired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			epoch := timeline(7)
			// The cycle opens unborn, so it bootstraps once before the drain
			// even begins; the mid-drain refusal is the second snapshot.
			h := newDeliveryHarness(epoch, 10, dataplane.ProjectionPosition{ProtocolVersion: projection.ProtocolVersion})
			h.log.snapshotAt = 9
			h.log.pages = [][]projection.Change{logChanges(t, 0, 3), logChanges(t, 2, 1), nil}
			h.mirror.snapshotCalls = []snapshotScript{ack(0), ack(2)}
			h.mirror.changeCalls = []changeScript{{err: tt.refusal}, changeAck(3)}

			if err := h.run(t); err != nil {
				t.Fatalf("Reconcile returned %v, want the refusal to heal inside the cycle", err)
			}
			if got, want := h.mirror.snapshots, 2; got != want {
				t.Fatalf("got %d snapshots, want %d — the bootstrap and the mid-drain heal", got, want)
			}
			if got, want := h.mirror.deliveries, 2; got != want {
				t.Fatalf("got %d batch deliveries, want %d — the refused batch and the page after the heal", got, want)
			}
			if got, want := h.log.afters, []uint64{0, 2, 3}; !equalRevisions(got, want) {
				t.Fatalf("got ChangesAfter calls at %v, want %v — the drain resumes from the heal snapshot's ack, not from where it broke", got, want)
			}
			if got, want := *h.trace, []string{
				"head", "position", "snapshot", "deliver:snapshot", "changes:0", "deliver:changes",
				"snapshot", "deliver:snapshot", "head", "changes:2", "deliver:changes", "changes:3",
			}; !equalStrings(*h.trace, want) {
				t.Fatalf("got trace %v, want %v — one cycle, one heal, the epoch re-read with the snapshot, and no second read of the position", got, want)
			}
		})
	}
}

func TestTheDrainSpeaksTheRemintedEpochAfterAHeal(t *testing.T) {
	// The restore procedure in miniature: the mirror was reset under the
	// drain and the producer's epoch re-minted over it (ADR 0007's operator
	// step), so the drain's first delivery after the heal must speak the
	// re-minted epoch — the snapshot was cut against the log as it stands
	// now, and a loop that kept the epoch it read at cycle open would refuse,
	// heal, refuse, until the deadline cut it off: bounded, but pointless.
	// The head re-read after the heal is what makes the next batch name the
	// timeline's current word.
	oldEpoch, mintedEpoch := timeline(9), timeline(10)
	position := dataplane.ProjectionPosition{
		ProtocolVersion: projection.ProtocolVersion,
		Bootstrapped:    true,
		Epoch:           oldEpoch,
	}
	h := newDeliveryHarness(oldEpoch, 10, position)
	h.log.snapshotAt = 5
	h.log.switchAfter = 1 // the cycle's opening read; the heal's re-read is the second
	rehead := projection.Head{Epoch: mintedEpoch, LastRevision: 10}
	h.log.switchHead = &rehead
	h.log.pages = [][]projection.Change{logChanges(t, 0, 3), logChanges(t, 5, 2), nil}
	h.mirror.snapshotCalls = []snapshotScript{ack(5)}
	h.mirror.changeCalls = []changeScript{{err: dataplane.ErrProjectionSnapshotRequired}, changeAck(7)}

	if err := h.run(t); err != nil {
		t.Fatalf("Reconcile returned %v, want the refusal healed and the drain resumed under the re-minted epoch", err)
	}
	if got, want := h.mirror.snapshots, 1; got != want {
		t.Fatalf("got %d snapshots, want %d — one heal, not a refusal loop", got, want)
	}
	if got, want := h.mirror.deliveries, 2; got != want {
		t.Fatalf("got %d batch deliveries, want %d — the refused delivery and the one after the heal", got, want)
	}
	if got, want := h.mirror.deliveredEpochs[2], mintedEpoch; got != want {
		t.Fatalf("got batch epoch %q, want %q — the batch after the heal speaks the re-minted timeline, not the dead one", got, want)
	}
	if got, want := h.mirror.batchFroms, []uint64{0, 5}; !equalRevisions(got, want) {
		t.Fatalf("got batches from %v, want %v — the resume point is the heal snapshot's ack", got, want)
	}
	if got, want := *h.trace, []string{
		"head", "position", "changes:0", "deliver:changes",
		"snapshot", "deliver:snapshot", "head", "changes:5", "deliver:changes", "changes:7",
	}; !equalStrings(*h.trace, want) {
		t.Fatalf("got trace %v, want %v — the heal re-read the head with the snapshot it cut", got, want)
	}
}

func TestVersionAndShapeRefusalsHaltTheCycleLoudly(t *testing.T) {
	// A version this build does not speak and a grammar this build broke are
	// not delivery problems, so no snapshot can fix them and no retry survives
	// them. The loop must fail the cycle with the sentinel intact and stop
	// where it stands: nothing skipped past, nothing re-sent, nothing
	// bootstrapped in a panic. An error that quietly lost its sentinel would
	// turn an operator's page into a redelivery loop.
	tests := []struct {
		name     string
		refusal  error
		sentinel error
	}{
		{
			name:     "the other side speaks an unknown protocol version",
			refusal:  dataplane.ErrProjectionUnsupportedVersion,
			sentinel: dataplane.ErrProjectionUnsupportedVersion,
		},
		{
			name:     "the answer is outside the protocol's grammar",
			refusal:  dataplane.ErrProjectionShape,
			sentinel: dataplane.ErrProjectionShape,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			epoch := timeline(8)
			position := dataplane.ProjectionPosition{
				ProtocolVersion: projection.ProtocolVersion,
				Bootstrapped:    true,
				Epoch:           epoch,
			}
			h := newDeliveryHarness(epoch, 10, position)
			h.log.pages = [][]projection.Change{logChanges(t, 0, 3), nil}
			h.mirror.changeCalls = []changeScript{{err: tt.refusal}}

			err := h.run(t)
			if !errors.Is(err, tt.sentinel) {
				t.Fatalf("got %v, want an error wrapping %v — the sentinel is the operator's whole diagnosis", err, tt.sentinel)
			}
			if h.mirror.snapshots != 0 {
				t.Fatalf("got %d snapshots, want 0 — this refusal means the build is wrong, and a snapshot is not an answer to it", h.mirror.snapshots)
			}
			if got, want := h.mirror.deliveries, 1; got != want {
				t.Fatalf("got %d batch deliveries, want %d — the refused batch is the last thing that happens; no skip, no retry in the cycle", got, want)
			}
			if got, want := h.log.afters, []uint64{0}; !equalRevisions(got, want) {
				t.Fatalf("got ChangesAfter calls at %v, want %v — the loop stopped instead of reading past the refusal", got, want)
			}
		})
	}
}

func TestAnUnavailableDataPlaneFailsTheCycleWithoutBootstrapping(t *testing.T) {
	// Unavailability means the answer is unknown, and the protocol's answer to
	// an unknown answer is the next tick — not an improvised snapshot in the
	// dark. The cycle fails with the sentinel carried whole, and the test
	// holds the mirror's bootstrap count at zero to prove the loop did not
	// promote a transport failure into a re-bootstrap.
	epoch := timeline(9)
	position := dataplane.ProjectionPosition{
		ProtocolVersion: projection.ProtocolVersion,
		Bootstrapped:    true,
		Epoch:           epoch,
	}
	h := newDeliveryHarness(epoch, 10, position)
	h.log.pages = [][]projection.Change{logChanges(t, 0, 3), nil}
	h.mirror.changeCalls = []changeScript{{err: dataplane.ErrProjectionUnavailable}}

	err := h.run(t)
	if !errors.Is(err, dataplane.ErrProjectionUnavailable) {
		t.Fatalf("got %v, want an error wrapping ErrProjectionUnavailable — the unknown answer belongs to the next cycle, not to this one's improvisation", err)
	}
	if h.mirror.snapshots != 0 {
		t.Fatalf("got %d snapshots, want 0 — unavailability fails the cycle, it does not trigger a bootstrap", h.mirror.snapshots)
	}
	if got, want := h.mirror.deliveries, 1; got != want {
		t.Fatalf("got %d batch deliveries, want %d — the failed delivery is the cycle's last act", got, want)
	}
}

func TestLogReadFailuresFailTheCycleWithTheirCauseNamed(t *testing.T) {
	// Every read the cycle makes can fail, and the failure must surface with
	// its half named — head, position, snapshot cut, feed page — and with the
	// underlying cause still attached, because "the loop failed" without the
	// cause is a bug report nobody can act on. What must not happen is any
	// delivery on top of a half-read world.
	tests := []struct {
		name     string
		position dataplane.ProjectionPosition
		script   func(h *deliveryHarness)
		wantText string
		cause    error
	}{
		{
			name:     "the head read fails",
			position: dataplane.ProjectionPosition{ProtocolVersion: projection.ProtocolVersion, Bootstrapped: true, Epoch: timeline(10)},
			script: func(h *deliveryHarness) {
				h.log.headErr = errFakeHead
			},
			wantText: "read the log's head",
			cause:    errFakeHead,
		},
		{
			name:     "the position read fails",
			position: dataplane.ProjectionPosition{},
			script: func(h *deliveryHarness) {
				h.mirror.positionErr = errFakePosition
			},
			wantText: "read the mirror's position",
			cause:    errFakePosition,
		},
		{
			name:     "the snapshot cut fails during a forced bootstrap",
			position: dataplane.ProjectionPosition{ProtocolVersion: projection.ProtocolVersion},
			script: func(h *deliveryHarness) {
				h.log.snapshotErr = errFakeSnapshot
			},
			wantText: "cut the snapshot",
			cause:    errFakeSnapshot,
		},
		{
			name: "the feed read fails mid-drain",
			position: dataplane.ProjectionPosition{
				ProtocolVersion: projection.ProtocolVersion,
				Bootstrapped:    true,
				Epoch:           timeline(10),
				AppliedRevision: 7,
			},
			script: func(h *deliveryHarness) {
				h.log.changesErr = errFakeChanges
			},
			wantText: "read the log after revision 7",
			cause:    errFakeChanges,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newDeliveryHarness(timeline(10), 10, tt.position)
			tt.script(h)

			err := h.run(t)
			if !errors.Is(err, tt.cause) {
				t.Fatalf("got %v, want an error wrapping %v — the cause travels with the cycle's failure", err, tt.cause)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("got error text %q, want it to name the failed half (%q)", err.Error(), tt.wantText)
			}
			if h.mirror.snapshots != 0 && tt.cause != errFakeSnapshot {
				t.Fatalf("got %d snapshots, want 0 — a failed read delivers nothing", h.mirror.snapshots)
			}
		})
	}
}

func TestAGappedLogFailsTheBatchAssemblyInsteadOfDeliveringIt(t *testing.T) {
	// Contiguity is checked by the domain, in this process, before any bytes
	// exist — and the check is the guard that keeps a corrupted log from being
	// delivered as a batch the consumer must refuse. A page carrying
	// revisions 5 and 7 cannot be a batch, so the cycle must fail on the
	// assembly, loudly, with the mirror never having been asked.
	epoch := timeline(11)
	position := dataplane.ProjectionPosition{
		ProtocolVersion: projection.ProtocolVersion,
		Bootstrapped:    true,
		Epoch:           epoch,
		AppliedRevision: 4,
	}
	h := newDeliveryHarness(epoch, 10, position)
	h.log.pages = [][]projection.Change{{logChange(t, 5), logChange(t, 7)}}

	err := h.run(t)
	if err == nil {
		t.Fatal("got no error from a gapped page, want the assembly to refuse it — a broken batch must never reach the mirror")
	}
	if !strings.Contains(err.Error(), "assemble the batch after revision 4") {
		t.Fatalf("got error text %q, want it to name the failed assembly at revision 4", err.Error())
	}
	if got, want := h.mirror.deliveries, 0; got != want {
		t.Fatalf("got %d batch deliveries, want %d — the batch that could not be built is the batch that is not sent", got, want)
	}
	if got, want := h.mirror.snapshots, 0; got != want {
		t.Fatalf("got %d snapshots, want %d — a corrupted log is not healed, it is reported", got, want)
	}
}

func TestACancelledContextStopsTheDrainBeforeItsNextPage(t *testing.T) {
	// The drain checks the context at the top of every page, so a cycle whose
	// deadline dies mid-feed stops between deliveries rather than marching on
	// with a dead context. The test cancels from inside the first batch's
	// acknowledgement — the deterministic moment that puts the cancellation
	// exactly on the boundary the guard owns.
	epoch := timeline(12)
	ctx, cancel := context.WithCancel(context.Background())
	position := dataplane.ProjectionPosition{
		ProtocolVersion: projection.ProtocolVersion,
		Bootstrapped:    true,
		Epoch:           epoch,
	}
	h := newDeliveryHarness(epoch, 10, position)
	h.log.pages = [][]projection.Change{logChanges(t, 0, 3), logChanges(t, 3, 3), nil}
	h.mirror.changeCalls = []changeScript{{ack: dataplane.ProjectionAck{AppliedRevision: 3}, cancelAfter: cancel}, changeAck(6)}

	err := h.producer.Reconcile(ctx, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want an error wrapping context.Canceled — a cancelled cycle reports its cancellation", err)
	}
	if got, want := len(h.log.afters), 1; got != want {
		t.Fatalf("got %d feed reads, want %d — the loop stopped before asking for the next page", got, want)
	}
	if got, want := h.mirror.deliveries, 1; got != want {
		t.Fatalf("got %d batch deliveries, want %d — the first page was delivered, the second was never attempted", got, want)
	}
}

func TestTheSnapshotAcknowledgementIsTheBootstrapResumePoint(t *testing.T) {
	// The snapshot's own revision and the mirror's committed position are two
	// different numbers, and only the second one is the drain's resume point.
	// The cut here is taken at 100 while the mirror acks 42; the next feed
	// read must ask for what follows 42 — the acknowledgement is authoritative
	// precisely because the mirror, not the producer, is the keeper of the
	// position.
	epoch := timeline(13)
	h := newDeliveryHarness(epoch, 100, dataplane.ProjectionPosition{ProtocolVersion: projection.ProtocolVersion})
	h.log.snapshotAt = 100
	h.log.pages = [][]projection.Change{logChanges(t, 42, 2), nil}
	h.mirror.snapshotCalls = []snapshotScript{ack(42)}
	h.mirror.changeCalls = []changeScript{changeAck(44)}

	if err := h.run(t); err != nil {
		t.Fatalf("Reconcile returned %v, want a clean cycle", err)
	}
	if got, want := h.log.afters[0], uint64(42); got != want {
		t.Fatalf("got the first feed read at revision %d, want %d — the ack names the resume point, not the snapshot cut's own boundary", got, want)
	}
	if got, want := h.mirror.snapshotRevisions, []uint64{100}; !equalRevisions(got, want) {
		t.Fatalf("got snapshot cut at %v, want %v — the two numbers are deliberately different", got, want)
	}
}

func equalRevisions(got, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
