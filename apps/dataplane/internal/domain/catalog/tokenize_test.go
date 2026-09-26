package catalog

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The canonical v1 input counter's tests pin the three properties the hold
// depends on: the count is the UTF-8 byte length of the WHOLE body (so the
// envelope's tools and schemas are priced, not only the message strings), it
// is stated in bytes (so a multibyte script holds against its real wire
// size), and it is an identity — the same body counts the same on every call,
// and never below its own byte length, so a hold priced from it can only be
// too large, never too small.

func TestCountInputTokensCountsTheWholeBodyInBytes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "an empty body", body: ""},
		{name: "a single ascii character", body: "x"},
		{name: "an ascii envelope", body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{name: "a cjk sentence in bytes, not runes", body: "你好世界"},
		{name: "an emoji in bytes, not runes", body: "🔑"},
		{name: "a family emoji in bytes, not runes", body: "👨‍👩‍👧"},
		{name: "accented letters in bytes, not runes", body: "éé"},
		{name: "whitespace is bytes too", body: "  \t\n  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CountInputTokens([]byte(tt.body))
			if want := int64(len(tt.body)); got != want {
				t.Errorf("CountInputTokens(%q) = %d, want the body's %d bytes", tt.body, got, want)
			}
		})
	}
}

// The scope property, as its own test: the count is over the whole envelope
// and not the message strings alone. A body whose tools and schemas dwarf its
// one-line message must count what the provider will tokenize — everything —
// and a future edit that reads only the message strings would halve this
// number.
func TestCountInputTokensCountsTheEnvelopeNotOnlyTheMessages(t *testing.T) {
	messagesOnly := `{"messages":[{"role":"user","content":"hi"}]}`
	withTools := `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"search","description":"search the web","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}}]}`

	if CountInputTokens([]byte(withTools)) <= CountInputTokens([]byte(messagesOnly)) {
		t.Errorf("CountInputTokens counted the tools-bearing body at %d and the messages-only body at %d; the whole envelope must be counted",
			CountInputTokens([]byte(withTools)), CountInputTokens([]byte(messagesOnly)))
	}
}

// TestCountInputTokensCountsUTF8BytesNotRunes states the distinction as its
// own test, because the value it protects is the one a future edit to a rune
// count would silently break: the same body, two different numbers.
func TestCountInputTokensCountsUTF8BytesNotRunes(t *testing.T) {
	const cjk = "模型"
	got := CountInputTokens([]byte(cjk))
	if got != int64(len(cjk)) {
		t.Errorf("CountInputTokens(%q) = %d, want %d bytes", cjk, got, len(cjk))
	}
	if runes := int64(utf8.RuneCountInString(cjk)); got == runes {
		t.Errorf("CountInputTokens(%q) = %d, which is the rune count; the count is in bytes", cjk, got)
	}
}

// TestCountInputTokensNeverUnderCountsBytes is the direction property: for
// every body the tests can build, the count is exactly the byte length —
// which is at least the rune count and at least any tokenizer's token count,
// so a hold priced from it can only be too large, never too small.
func TestCountInputTokensNeverUnderCountsBytes(t *testing.T) {
	bodies := []string{
		"", "a", "hello world", "你好世界", "🔑", "👨‍👩‍👧‍👦", "ééé",
		strings.Repeat("x", 4096), strings.Repeat("好", 1024),
		"mixed 混合 🔑 content", "  \t\n  ", "!@#$%^&*()", "Ω≈ç√∫˜µ",
		`{"messages":["多","byte"]}`,
	}
	for _, body := range bodies {
		counted := CountInputTokens([]byte(body))
		if want := int64(len(body)); counted != want {
			t.Fatalf("CountInputTokens(%q) = %d, want the body's %d bytes", body, counted, want)
		}
		if runes := int64(utf8.RuneCountInString(body)); counted < runes {
			t.Errorf("CountInputTokens(%q) = %d, below its %d runes; the count must be in bytes", body, counted, runes)
		}
	}
}
