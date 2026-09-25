package postgres

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// The fact-feed cursor: "v1.<epoch>.<append_seq>".
//
// The cursor is this adapter's own format and nobody else's — the port
// promises opacity, and opacity is what makes the format free to change. What
// it is, precisely: the epoch of the stream identity the position belongs to,
// and the last append sequence delivered under it. The epoch half is what
// makes a cursor honest after a database loses its rows — the stream row is
// minted by the server on first append, never by a migration, so a recreated
// database carries a new epoch and every cursor minted by the old one names a
// stream that no longer exists.
//
// Every refusal here is usagefacts.ErrCursorExpired, and that is a decision,
// not a convenience. A malformed or unknown cursor is a position this Data
// Plane cannot place, and the protocol's answer for a placeable-nowhere
// position is 410 cursor_expired — the consumer stops and a human decides
// about the gap. 400 would be the same read retried forever: the façade
// translates a management 4xx into a client-facing failure that tells the
// consumer its request was wrong, and a position that never becomes right
// would retry until someone reads the logs. See
// docs/architecture/cross-plane-protocols.md, which pins the 410.

// genesisCursor is the position before any fact exists. A consumer with no
// stored position sends an empty `after`, which means the same thing.
const genesisCursor = "v1.0"

// cursor is one decoded position: which stream, how far into it.
type cursor struct {
	epoch string
	seq   int64
}

// encodeCursor renders a position in its canonical spelling. It is the only
// writer of the format, which is what makes the decode's round-trip check
// meaningful.
func encodeCursor(epoch string, seq int64) string {
	return fmt.Sprintf("v1.%s.%d", epoch, seq)
}

// decodeCursor parses a cursor handed back by a caller — which may have kept
// it for a month, copied it through three systems, or mangled it in one — and
// refuses everything that is not exactly a position this stream minted.
//
// The check is stricter than "parses". A cursor that parses but does not
// round-trip through encodeCursor byte for byte — a leading zero in the
// sequence, a trailing space, an uppercase epoch — names a position by a
// spelling this adapter never issued, and answering it would teach consumers
// that any equivalent spelling works, then break them the day one of those
// spellings means something else. Canonical or refused; there is no
// normalising halfway.
func decodeCursor(after string) (cursor, error) {
	if after == "" {
		return cursor{epoch: "", seq: 0}, nil
	}
	if after == genesisCursor {
		return cursor{epoch: "", seq: 0}, nil
	}

	rest, ok := strings.CutPrefix(after, "v1.")
	if !ok {
		return cursor{}, expiredf("cursor %q is not a v1 position", after)
	}
	epoch, seqPart, ok := strings.Cut(rest, ".")
	if !ok || epoch == "" || seqPart == "" {
		return cursor{}, expiredf("cursor %q does not name an epoch and a sequence", after)
	}
	seq, err := strconv.ParseInt(seqPart, 10, 64)
	if err != nil || seq < 0 {
		return cursor{}, expiredf("cursor %q does not carry a sequence number", after)
	}
	if reencoded := encodeCursor(epoch, seq); reencoded != after {
		return cursor{}, expiredf("cursor %q is not in the canonical spelling", after)
	}
	return cursor{epoch: epoch, seq: seq}, nil
}

// expiredf builds the one error every cursor refusal is. The cause stays in
// the wrap for the log line; the sentinel is what the surface maps to 410.
func expiredf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", usagefacts.ErrCursorExpired, fmt.Sprintf(format, args...))
}
