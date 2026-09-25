package catalog

// CountInputTokens is admission's interim input-token counter: the UTF-8 byte
// length of every content part, summed. It is deliberately not a tokenizer.
//
// The hold is computed before any provider is called, so it needs a token
// count the request itself already carries, and the only number a request
// carries is bytes. A byte is the worst-case token unit for every mainstream
// tokenizer — no tokeniser produces more tokens than input bytes — so pricing
// the input at its byte count can only overstate the hold, never understate
// it, and an overstated hold costs a client a temporarily larger reservation,
// while an understated one is the under-covered hold the waterfall exists to
// make impossible. Multibyte scripts count in full: the sum is over bytes, not
// runes, so CJK and emoji content holds against its real wire size rather
// than a rune count that would be several times smaller.
//
// This function is the seam the real canonical tokenizer (B11) replaces: when
// that arrives, it takes this name's place as the single call site in
// admission, and the hold formula itself does not move. Nothing else may call
// it — it prices nothing, it bounds the reservation only.
//
// The validity contract is the pairing of two refusals: the contract's own
// `input_tokens >= 0` and NewRequest's `> 0`. A structurally valid request
// counts at least one byte, so zero here means there was no content at all —
// the caller answers invalid_request, and the zero that reaches the domain
// constructor is refused there a second time.
func CountInputTokens(contents []string) int64 {
	var total int64
	for _, content := range contents {
		total += int64(len(content))
	}
	return total
}
