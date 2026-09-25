package execution

import (
	"errors"
	"strings"
	"testing"
)

// The Idempotency-Key tests pin both directions of the grammar: the values
// the runtime stores byte-for-byte, and the refusals it answers
// invalid_request with. The header exists so a client retrying after a
// timeout does not execute twice (ADR 0004), so there is deliberately no
// "make it fit" path — no trimming, no truncation, no generated substitute —
// and the tests refuse every repair a future edit might be tempted to add.

func TestValidateIdempotencyKeyAcceptsTheWholeVisibleASCIIRange(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "one character", key: "x"},
		{name: "the lowest visible octet", key: "!"},
		{name: "the highest visible octet", key: "~"},
		{name: "punctuation and digits", key: "00000000-0000-0000-0000-000000000000"},
		{name: "every visible ASCII octet at once", key: allVisibleASCII()},
		{name: "the schema's 256-octet maximum", key: strings.Repeat("k", 256)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateIdempotencyKey(tt.key); err != nil {
				t.Errorf("ValidateIdempotencyKey(%q) error = %v, want nil", tt.key, err)
			}
		})
	}
}

func TestValidateIdempotencyKeyRefusesEverythingElse(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "an empty value", key: ""},
		{name: "a 257-octet value", key: strings.Repeat("k", 257)},
		{name: "a space", key: "key with space"},
		{name: "a leading space", key: " key"},
		{name: "a tab", key: "key\twith\ttabs"},
		{name: "a newline", key: "key\n"},
		{name: "a carriage return", key: "key\r\n"},
		{name: "DEL", key: "key\x7f"},
		{name: "a NUL", key: "key\x00"},
		{name: "an escape", key: "\x1b[31m"},
		{name: "a non-ascii letter", key: "clé"},
		{name: "an emoji", key: "🔑"},
		{name: "a cjk character", key: "键"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateIdempotencyKey(tt.key)
			if !errors.Is(err, ErrInvalidIdempotencyKey) {
				t.Fatalf("ValidateIdempotencyKey(%q) error = %v, want ErrInvalidIdempotencyKey", tt.key, err)
			}
		})
	}
}

// allVisibleASCII builds the 95-octet run 0x21..0x7E — every value the
// grammar allows, concatenated once.
func allVisibleASCII() string {
	var b strings.Builder
	for c := 0x21; c <= 0x7e; c++ {
		b.WriteByte(byte(c))
	}
	return b.String()
}
