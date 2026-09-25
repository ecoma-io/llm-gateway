package catalog

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The interim token counter's tests pin the two properties the hold depends
// on: the count is in BYTES (so a multibyte script holds against its real
// wire size) and it never under-counts (so a hold priced from it can only be
// too large, never too small). The canonical tokenizer that replaces this
// function (B11) must keep the second property; only the first is a
// placeholder's approximation.

func TestCountInputTokensSumsBytesAcrossContentParts(t *testing.T) {
	tests := []struct {
		name     string
		contents []string
		want     int64
	}{
		{name: "no content at all", contents: nil, want: 0},
		{name: "an empty content list", contents: []string{}, want: 0},
		{name: "one empty part", contents: []string{""}, want: 0},
		{name: "one ASCII part", contents: []string{"hello"}, want: 5},
		{name: "three ASCII parts", contents: []string{"one", "two", "three"}, want: 11},
		{name: "a part per message with punctuation", contents: []string{"user: ", "hi", "\n"}, want: 9},
		{name: "a CJK sentence in bytes, not runes", contents: []string{"你好世界"}, want: 12},
		{name: "an emoji in bytes, not runes", contents: []string{"🔑"}, want: 4},
		{name: "a family emoji in bytes, not runes", contents: []string{"👨‍👩‍👧"}, want: 18},
		{name: "mixed scripts", contents: []string{"hi ", "你好", " 🔑"}, want: 14},
		{name: "accented letters in bytes, not runes", contents: []string{"éé"}, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CountInputTokens(tt.contents)
			if got != tt.want {
				t.Errorf("CountInputTokens(%q) = %d, want %d", tt.contents, got, tt.want)
			}
		})
	}
}

// TestCountInputTokensCountsUTF8BytesNotRunes states the distinction as its
// own test, because the value it protects is the one a future edit to a rune
// count would silently break: the same content, two different numbers.
func TestCountInputTokensCountsUTF8BytesNotRunes(t *testing.T) {
	const cjk = "模型"
	got := CountInputTokens([]string{cjk})
	if got != int64(len(cjk)) {
		t.Errorf("CountInputTokens(%q) = %d, want %d bytes", cjk, got, len(cjk))
	}
	if runes := int64(utf8.RuneCountInString(cjk)); got == runes {
		t.Errorf("CountInputTokens(%q) = %d, which is the rune count; the count is in bytes", cjk, got)
	}
}

// TestCountInputTokensNeverUnderCountsBytes is the direction property: the
// counter is at least the number of bytes in the content, for every part
// shape the tests can build. A counter below the byte count would size a
// hold under the request, which is the one failure this interim function
// cannot have.
func TestCountInputTokensNeverUnderCountsBytes(t *testing.T) {
	parts := []string{
		"", "a", "hello world", "你好世界", "🔑", "👨‍👩‍👧‍👦", "ééé",
		strings.Repeat("x", 4096), strings.Repeat("好", 1024),
		"mixed 混合 🔑 content", "  \t\n  ", "!@#$%^&*()", "Ω≈ç√∫˜µ",
	}
	for _, part := range parts {
		counted := CountInputTokens([]string{part})
		if want := int64(len(part)); counted < want {
			t.Errorf("CountInputTokens(%q) = %d, below the %d bytes it must never under-count", part, counted, want)
		}
	}
	// And the additive direction: a list counts at least the sum of its
	// parts' bytes, which the parts-alone check would not catch.
	joined := CountInputTokens(parts)
	sum := 0
	for _, part := range parts {
		sum += len(part)
	}
	if joined < int64(sum) {
		t.Errorf("CountInputTokens(parts) = %d, below the %d bytes of the parts", joined, sum)
	}
}
