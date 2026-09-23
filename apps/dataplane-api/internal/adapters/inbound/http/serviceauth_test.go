package http

import (
	"context"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// TestConstantTimeEqualAnswersTheSameQuestionThePlainComparisonWould is the
// semantic half of the fixed-width comparison, and it is the half a test can
// reach.
//
// What cannot be asserted here is the timing property itself: a unit test that
// measured how long a refusal took would be measuring the machine it runs on,
// and it would be flaky in exactly the deployment where it mattered. What can
// be asserted is that normalising both sides through a digest did not change
// the answer — a credential equal to the configured one still compares equal,
// and one that differs at all compares unequal, including when the difference
// is nothing but length, which is the case the earlier implementation decided
// before comparing anything.
func TestConstantTimeEqualAnswersTheSameQuestionThePlainComparisonWould(t *testing.T) {
	const configured = "a-service-credential-for-tests"

	tests := []struct {
		name      string
		presented string
		want      bool
	}{
		{name: "an identical credential compares equal", presented: configured, want: true},
		{name: "a prefix does not", presented: configured[:len(configured)-1]},
		{name: "an extension does not", presented: configured + "x"},
		{name: "a same-length variation in the first byte does not", presented: "b" + configured[1:]},
		{name: "a same-length variation in the last byte does not", presented: configured[:len(configured)-1] + "z"},
		{name: "an empty credential does not", presented: ""},
		{name: "a much longer credential does not", presented: strings.Repeat("s", 4096)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := constantTimeEqual(tt.presented, configured); got != tt.want {
				t.Errorf("constantTimeEqual(%q, configured) = %v, want %v", tt.presented, got, tt.want)
			}
		})
	}
}

// TestTheAuthenticatorRefusesAnEmptyConfiguredSecret pins the one fail-open the
// comparison could otherwise have.
//
// Two empty strings are equal, so a deployment that configured no credential
// and a caller that presented none would match if the comparison were reached —
// an administrative surface open to anything that can reach the port, decided
// by the absence of a secret rather than by its presence. The guard that
// prevents it branches on the deployment's configuration and not on the
// request, so no caller can influence which path it takes.
func TestTheAuthenticatorRefusesAnEmptyConfiguredSecret(t *testing.T) {
	authenticator := NewServiceAuthenticator("")

	for _, presented := range []string{"", "anything at all"} {
		t.Run(presented, func(t *testing.T) {
			if caller, ok := authenticator.Authenticate(context.Background(), presented); ok {
				t.Errorf("Authenticate(%q) with no configured secret = %+v, true; want a refusal", presented, caller)
			}
		})
	}
}

// TestTheAuthenticatorReturnsTheOneNameItTrusts is the accepting half: the
// configured credential is admitted, and what it is admitted as is a constant
// rather than anything derived from the presented value. There is no identity
// system behind this port — one deployment, one peer — and a name that came
// from the request would be the first line of one.
func TestTheAuthenticatorReturnsTheOneNameItTrusts(t *testing.T) {
	authenticator := NewServiceAuthenticator(testCredential)

	caller, ok := authenticator.Authenticate(context.Background(), testCredential)
	if !ok {
		t.Fatalf("Authenticate() refused the configured credential")
	}
	if got, want := caller.Name, serviceCallerName; got != want {
		t.Errorf("Authenticate() name = %q, want %q", got, want)
	}

	// The compile-time half of the claim: the type this constructor returns is
	// the port the inbound adapter is handed, so a method that stops matching
	// the interface is a build failure here rather than a wiring surprise at the
	// composition root.
	var _ dataplane.Authenticator = serviceAuthenticator{}
}
