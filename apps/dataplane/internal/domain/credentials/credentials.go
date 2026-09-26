// The provider credential's model. A backend row never carries credential
// material — it carries a reference, and the reference's grammar is the whole
// of this package:
//
//	env:NAME
//
// The `env` scheme names an environment variable, and the material is read
// from the process environment at the moment of use — never at construction,
// never at snapshot build, never into any struct that outlives the call. A
// rotated variable is picked up by the next call, which is the property that
// makes resolution-at-use the only honest reading of the reference.
//
// What the grammar deliberately does not have: a second scheme. Every other
// way a runtime might be tempted to fetch a credential — a vault client, a
// file path, an inline literal — is a design decision that brings its own
// storage, its own redaction obligations and its own failure modes, and the
// grammar stays one scheme until one of those designs is actually made. An
// unparsable reference is a wiring defect at the composition root, and it is
// refused there, loudly, rather than resolved to nothing at call time.
//
// Secret is the material's one shape. It exists so the material has a type
// whose every accidental printing path is a redaction, and so the arch rule
// beside this package has a name to guard: no struct field anywhere in this
// module may nest a Secret, because fmt cannot invoke a Stringer on an
// unexported field and will print the raw bytes under %v. In this runtime the
// material's whole life is one function frame — resolved by the executor's
// credential closure, sent in one header, gone — and the type exists to keep
// that life that short.
package credentials

import (
	"errors"
	"strings"
)

// envScheme is the one scheme the reference grammar spells.
const envScheme = "env"

// redactedSecret is what every printing path renders instead of material.
const redactedSecret = "credentials.Secret(redacted)"

// Ref is one backend row's credentials reference: the parsed form of the
// `env:NAME` grammar. It names where material comes from and holds none.
type Ref struct {
	name string
}

// ParseRef reads one reference. The refusals are the composition root's
// wiring defects: a reference without a scheme, a scheme this runtime does
// not resolve, a variable name that cannot be spelled as one. All three are
// refused here rather than discovered as per-call authentication failures a
// snapshot later — an executor built over a reference nobody can resolve is
// a backend that fails every call forever, and that is a boot-time fact, not
// a runtime one.
//
// Every refusal names the grammar and never the value. A reference field is
// exactly where credential material ends up when an operator wiring a
// backend pastes it into the wrong column, and these errors are the ones the
// registry's skip-and-log prints — so the refusal must not turn one bad row
// into the material sitting in a log line.
func ParseRef(raw string) (Ref, error) {
	scheme, name, found := strings.Cut(raw, ":")
	if !found {
		return Ref{}, errors.New("credentials: a reference must name its scheme; the grammar is env:NAME")
	}
	if scheme != envScheme {
		return Ref{}, errors.New("credentials: the reference's scheme is not one this runtime resolves; the grammar is env:NAME")
	}
	if name == "" {
		return Ref{}, errors.New("credentials: an env reference must name its variable; the grammar is env:NAME")
	}
	if !validEnvName(name) {
		return Ref{}, errors.New("credentials: an env reference must name a variable spelled with letters, digits and underscores, not starting with a digit")
	}
	return Ref{name: name}, nil
}

// EnvName is the variable the reference names.
func (r Ref) EnvName() string { return r.name }

// LookupEnv is os.LookupEnv's shape, injected so resolution stays testable
// and so no package beside the composition root reads the process
// environment.
type LookupEnv func(string) (string, bool)

// Resolve reads the reference's material from the environment, at the moment
// this method is called. False means the variable is absent — the honest
// wiring report for a reference whose material is not deployed — and the
// caller classes the call authentication; it is never a reason to stop the
// process, because the variable may arrive with the next rotation.
func (r Ref) Resolve(lookup LookupEnv) (Secret, bool) {
	material, ok := lookup(r.name)
	if !ok {
		return Secret{}, false
	}
	return Secret{material: material}, true
}

// Secret is one provider credential's material, resolved from the
// environment for one call. String and GoString redact, so a log line, an
// error wrap or a %v shows the marker instead of the material; MarshalText
// refuses, so an accidental JSON round-trip cannot smuggle it anywhere.
//
// That net has one structural limit, recorded here so the next aggregate
// avoids it, and enforced by the arch test beside this package rather than
// trusted to it: fmt cannot invoke methods on unexported fields, so a Secret
// stored AS A STRUCT FIELD dumps its raw bytes under %v regardless of these
// methods. The redaction holds only while Secret values travel as
// themselves — arguments, returns, interface values — never nested in
// another struct. Nothing today nests one; nothing later may. Go cannot
// zero the material once resolved: the garbage collector may retain copies,
// so never logging and never nesting are the working mitigations, not
// erasure.
type Secret struct {
	material string
}

// Material is the one reader: the bytes as the executor's header build
// wants them. Every other use of a Secret is the redacted one.
func (s Secret) Material() string { return s.material }

// IsEmpty reports whether the material is the empty string — a variable
// deployed but set empty, which is the same as absent for a bearer header's
// purposes, and which the caller treats as absence rather than as a claim.
func (s Secret) IsEmpty() bool { return s.material == "" }

// String implements fmt.Stringer with a redaction, so every accidental
// printing path — %v, %s, error wrapping, struct dumps — shows a marker
// instead of credential material.
func (s Secret) String() string { return redactedSecret }

// GoString covers the %#v debugging verb with the same redaction.
func (s Secret) GoString() string { return redactedSecret }

// MarshalText refuses: any encoder that reaches for textual form (JSON,
// maps, structured logs) gets an error rather than credential material.
func (s Secret) MarshalText() ([]byte, error) {
	return nil, errors.New("credentials: secret material must never be serialised")
}

// validEnvName reports whether a name can be spelled as an environment
// variable: non-empty, letters, digits and underscores, not starting with a
// digit. The grammar's own words are the test's authority — a variable the
// shell cannot set is a variable no deployment can deploy.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' {
			continue
		}
		if r >= '0' && r <= '9' && i > 0 {
			continue
		}
		return false
	}
	return true
}
