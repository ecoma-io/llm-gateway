package catalog

// CountInputTokens is the canonical v1 input-token count: the UTF-8 byte
// length of the whole admitted request body. It is deliberately not a
// tokenizer.
//
// The hold is computed before any provider is called, so it needs a token
// count the request itself already carries, and the only number a request
// carries is bytes. A byte is the worst-case token unit for every mainstream
// tokenizer — no tokenizer produces more tokens than input bytes — so pricing
// the input at its byte count can only overstate the hold, never understate
// it, and an overstated hold costs a client a temporarily larger reservation,
// while an understated one is the under-covered hold the waterfall exists to
// make impossible. Multibyte scripts count in full: the count is over bytes,
// not runes, so CJK and emoji content holds against its real wire size rather
// than a rune count that would be several times smaller.
//
// The count is over the whole admitted body, not the message strings alone,
// and that scope is the point: a chat completion's input to the provider is
// everything in the envelope — the tools and their schemas, the response
// format, the parameter passthrough — and a provider tokenizes and bills all
// of it. A counter that read only the message strings would systematically
// understate every request that carries tools, and the hold it sized would
// not cover the usage the provider reports back. The envelope's few
// structural keys are noise next to that exposure, and the direction of the
// error stays the safe one.
//
// This is the canonical v1 rule, shared with the delivery counter in the
// accounting package: byte length now, a real tokenizer later replacing the
// bodies of both counters with their signatures unchanged. Nothing else may
// call it — it prices nothing, it bounds the reservation only.
//
// The validity contract is the pairing of two refusals: the contract's own
// `input_tokens >= 0` and NewRequest's `> 0`. An empty body cannot reach this
// function — nothing unparsed arrives with zero bytes — so the emptiness that
// matters is a body with no messages at all, and that refusal lives at the
// admission use case, which checks the message list directly rather than
// inferring it from a count.
func CountInputTokens(body []byte) int64 {
	return int64(len(body))
}
