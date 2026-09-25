package execution

import (
	"errors"
	"fmt"
)

// The Idempotency-Key grammar. The header is required on this surface and is
// an opaque client-chosen string: the runtime never parses it into a structure,
// never normalises it, and compares it byte-for-byte, so the only question
// worth asking is whether it is a string this runtime is willing to store in
// an index and echo back on a replay.
//
//   - visible ASCII, 0x21 through 0x7E: no space, no control character, no
//     DEL, nothing non-ASCII. Every HTTP/1.1 and HTTP/2 header value is an
//     opaque byte sequence, so the client could present anything; this is the
//     line that says the runtime will not.
//   - 1 through 256 octets, the schema's own bound, measured in octets because
//     a header bound in characters is two different numbers for two different
//     clients.
//
// The bound is why a key is refused rather than truncated: truncation would
// make two distinct client keys one stored key, and a collision here is a
// replay answered for the wrong request. Rejecting a too-long key is
// invalid_request, never a truncation and never a generated substitute — ADR
// 0004's whole reason the header exists is that a client retrying after a
// timeout must not execute twice, and a gateway that mints a key the client
// never chose has broken exactly that guarantee.

// maxIdempotencyKeyOctets is the schema's bound, restated so the domain check
// and the contract's maxLength can disagree only in a test.
const maxIdempotencyKeyOctets = 256

// ErrInvalidIdempotencyKey is a presented Idempotency-Key outside the grammar
// above. It is the one rejection the application turns into
// invalid_request_error with param "idempotency_key" — the client can see
// which header it got wrong, because the header is not a secret and the fix is
// to send another value. (The same wire shape is returned for a key that is
// well-formed but was already spent on a different body; that is
// idempotency_conflict, a different detail entirely, and the two are never
// merged.)
var ErrInvalidIdempotencyKey = errors.New("execution: idempotency key is outside the accepted grammar")

// ValidateIdempotencyKey refuses a header value the runtime will not store.
// The runtime's copy of an accepted key is exactly the octets that arrived —
// no trimming, no case folding, no re-encoding — because the stored key is
// what a replay's byte-for-byte comparison will meet again.
func ValidateIdempotencyKey(key string) error {
	if len(key) == 0 || len(key) > maxIdempotencyKeyOctets {
		return fmt.Errorf("%w: length %d", ErrInvalidIdempotencyKey, len(key))
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return fmt.Errorf("%w: octet %d is not visible ASCII", ErrInvalidIdempotencyKey, i)
		}
	}
	return nil
}
