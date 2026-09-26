package credentials

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestParseRefReadsTheEnvGrammar walks the reference grammar: the one scheme
// this runtime resolves, and every shape that is refused as the composition
// root's wiring defect rather than discovered as a per-call authentication
// failure.
func TestParseRefReadsTheEnvGrammar(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		refusal string
	}{
		{name: "a plain env reference reads", raw: "env:PROVIDER_API_KEY", want: "PROVIDER_API_KEY"},
		{name: "underscores and digits spell a variable", raw: "env:KEY_V2", want: "KEY_V2"},
		{
			name:    "a schemeless reference names nothing resolvable",
			raw:     "creds/main",
			refusal: "a reference must name its scheme; the grammar is env:NAME",
		},
		{
			name:    "a foreign scheme is not one this runtime resolves",
			raw:     "vault:secret/provider",
			refusal: "the reference's scheme is not one this runtime resolves; the grammar is env:NAME",
		},
		{
			name:    "an env reference must name its variable",
			raw:     "env:",
			refusal: "an env reference must name its variable; the grammar is env:NAME",
		},
		{
			name:    "a variable cannot start with a digit",
			raw:     "env:1KEY",
			refusal: "must name a variable spelled with letters, digits and underscores, not starting with a digit",
		},
		{
			name:    "a variable cannot carry separators",
			raw:     "env:PROVIDER-KEY",
			refusal: "must name a variable spelled with letters, digits and underscores, not starting with a digit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ref, err := ParseRef(test.raw)
			if test.refusal != "" {
				if err == nil {
					t.Fatalf("ParseRef(%q) = %v, want refusal %q", test.raw, ref, test.refusal)
				}
				if got := err.Error(); !strings.Contains(got, test.refusal) {
					t.Errorf("ParseRef(%q) error = %q, want it to carry %q", test.raw, got, test.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q) returned error: %v", test.raw, err)
			}
			if ref.EnvName() != test.want {
				t.Errorf("EnvName() = %q, want %q", ref.EnvName(), test.want)
			}
		})
	}
}

// TestParseRefRefusalsNeverEchoTheReference: a reference field is exactly
// where credential material ends up when an operator pastes it into the
// wrong column, and ParseRef's errors are the ones the executor registry's
// skip-and-log prints — so no refusal may carry a fragment of the value it
// refused. The grammar is named; the value never is.
func TestParseRefRefusalsNeverEchoTheReference(t *testing.T) {
	materialShaped := []string{
		"pasted-material-0001",       // raw material pasted whole: no scheme at all
		"env:pasted-material-0001",   // material behind the env scheme: a bad variable name
		"vault:pasted-material-0001", // a foreign scheme carrying material
		"pasted-material:0001",       // material whose first colon makes a bogus scheme
	}
	for _, raw := range materialShaped {
		_, err := ParseRef(raw)
		if err == nil {
			t.Fatalf("ParseRef accepted %q, want refusal", raw)
		}
		if got := err.Error(); strings.Contains(got, "pasted-material") {
			t.Errorf("ParseRef refusal %q echoes the refused value — the grammar is named, never the value", got)
		}
	}
}

// TestResolveReadsTheEnvironmentAtCallTime pins the property the grammar
// exists for: the lookup happens when Resolve is called, so a variable the
// environment gains later is picked up by the next call, and absence is a
// report the caller classes, not an error.
func TestResolveReadsTheEnvironmentAtCallTime(t *testing.T) {
	ref, err := ParseRef("env:PROVIDER_API_KEY")
	if err != nil {
		t.Fatalf("ParseRef returned error: %v", err)
	}
	absent := map[string]string{}
	lookup := func(name string) (string, bool) {
		value, ok := absent[name]
		return value, ok
	}

	if secret, ok := ref.Resolve(lookup); ok {
		t.Fatalf("Resolve over an absent variable = (%v, true), want absence", secret)
	}

	absent["PROVIDER_API_KEY"] = "first-material"
	secret, ok := ref.Resolve(lookup)
	if !ok {
		t.Fatal("Resolve over a present variable reported absence")
	}
	if secret.Material() != "first-material" {
		t.Errorf("Material() = %q, want the variable's value at call time", secret.Material())
	}

	// Rotation: the variable changes, the next call reads the new value.
	absent["PROVIDER_API_KEY"] = "second-material"
	if secret, ok = ref.Resolve(lookup); !ok || secret.Material() != "second-material" {
		t.Errorf("Resolve after rotation = (%v, %v), want the rotated value", secret, ok)
	}
}

// TestResolveTreatsAnEmptyVariableAsPresentButEmpty: an empty variable is a
// deployment's confession, not a parse failure — Resolve reports it present,
// and the caller's IsEmpty check is what turns it into the absence a bearer
// header cannot carry.
func TestResolveTreatsAnEmptyVariableAsPresentButEmpty(t *testing.T) {
	ref, err := ParseRef("env:PROVIDER_API_KEY")
	if err != nil {
		t.Fatalf("ParseRef returned error: %v", err)
	}
	secret, ok := ref.Resolve(func(name string) (string, bool) { return "", true })
	if !ok {
		t.Fatal("Resolve reported absence for a variable set empty; an empty variable is present and empty")
	}
	if !secret.IsEmpty() {
		t.Errorf("IsEmpty() = false for empty material, want true")
	}
}

// TestSecretRedactsEveryPrintingPath walks the net: Stringer, %#v, fmt of a
// value holding one, and the JSON refusal — every path a log line or a
// snapshot might reach for renders the marker or errors, never the material.
func TestSecretRedactsEveryPrintingPath(t *testing.T) {
	secret := Secret{material: "bearer-material-nobody-may-read"}

	if got := secret.String(); got != "credentials.Secret(redacted)" {
		t.Errorf("String() = %q, want the redaction", got)
	}
	if got := fmt.Sprintf("%v", secret); got != "credentials.Secret(redacted)" {
		t.Errorf("%%v = %q, want the redaction", got)
	}
	// The verb itself is under test: the secret travels through an interface
	// so fmt's %s dispatch is what runs here, not the direct String() call
	// the two lines above already covered.
	if got := fmt.Sprintf("%s", any(secret)); got != "credentials.Secret(redacted)" {
		t.Errorf("%%s = %q, want the redaction", got)
	}
	if got := fmt.Sprintf("%#v", secret); got != "credentials.Secret(redacted)" {
		t.Errorf("%%#v = %q, want the redaction", got)
	}
	encoded, err := json.Marshal(secret)
	if err == nil {
		t.Fatalf("json.Marshal(secret) = %s, want a refusal", encoded)
	}
	if got := secret.Material(); got != "bearer-material-nobody-may-read" {
		t.Errorf("Material() = %q, want the one reader to read", got)
	}
}
