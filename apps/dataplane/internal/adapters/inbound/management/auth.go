package management

import (
	"crypto/sha256"
	"crypto/subtle"
	stdhttp "net/http"
	"strings"
)

// authorizationHeader is the header a management caller presents its service
// credential in. This listener's caller is dataplane-api, not the Control Plane
// that reaches it, so the scheme is the private protocol's rather than
// api/openapi/dataplane.yaml's — the two are shaped alike and carry different
// secrets, and each hop checks its own (docs/architecture/cross-plane-protocols.md).
const authorizationHeader = "Authorization"

// credentialScheme is the authorization scheme this surface accepts. It is
// spelled once so the contract's scheme and the parser cannot disagree.
const credentialScheme = "Bearer"

// authorised reports whether a request carries the credential this surface was
// configured with.
//
// A management call authenticates as a *service*: the caller is a peer
// application inside the deployment, and the identity it presents is the
// deployment's own shared secret. It is deliberately not a user session and not
// a customer API key — a browser credential on this surface would make an
// operator's account an administrative identity, and a customer's LLM key would
// let anyone who can make an inference call also read the usage feed. Neither is
// a service identity, so neither is accepted (ADR 0006 §9).
//
// An empty configured credential authenticates nobody. This is the fail-closed
// half of the boundary: the composition root will not build a listener without
// one — config.Load refuses the process — but a constructor must not be the
// place where the deployment's security posture is decided either, because it
// can be called from a test or from a composition root that has not been written
// yet. The listener that answers 401 to everything is the safe outcome of that
// mistake; a listener that treated "no credential configured" as "no credential
// required" is the other one, and it is one word away.
//
// The comparison is constant-time over fixed-width digests. Comparing the
// strings directly would leak the credential's length through the first
// mismatching byte, which is enough to recover a secret one byte at a time given
// enough attempts, and this surface is on a network an attacker inside the
// deployment may already reach.
func authorised(r *stdhttp.Request, credential string) bool {
	if credential == "" {
		return false
	}
	presented, ok := bearerCredential(r)
	if !ok {
		return false
	}
	return constantTimeEqual(presented, credential)
}

// bearerCredential extracts the credential from the Authorization header.
//
// A request that presents the header twice is refused rather than resolved:
// there is no defensible rule for which of two credentials a caller meant, and
// accepting either would let a caller that guessed one of them wrong believe it
// had been read as the other. The remaining rules are RFC 6750's: the scheme is
// `Bearer`, compared case-insensitively, and the credential carries no
// whitespace.
func bearerCredential(r *stdhttp.Request) (string, bool) {
	values := r.Header.Values(authorizationHeader)
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], credentialScheme) {
		return "", false
	}
	return parts[1], true
}

// constantTimeEqual compares two secrets without leaking where they differ.
//
// Both sides are hashed first so the comparison is over a fixed width: a
// constant-time comparison of two different-length inputs still returns at the
// first byte, and the length of the credential is exactly the kind of thing that
// should not be observable from outside.
func constantTimeEqual(presented, credential string) bool {
	presentedSum := sha256.Sum256([]byte(presented))
	credentialSum := sha256.Sum256([]byte(credential))
	return subtle.ConstantTimeCompare(presentedSum[:], credentialSum[:]) == 1
}
