package accounting

// CountDeliveredTokens is the gateway's count of what reached the client,
// taken over the bytes the reply recorded.
//
// It is deliberately a byte-length rule, stated here rather than guessed at
// each call site: this is the canonical v1 delivery count, the rule the
// input counter in the catalog package shares, and every settlement's
// delivery figure and every attempt's delivery count comes from this one
// function — so the number an auditor re-derives from a fact is the number
// this build published, whatever its precision. A real tokenizer, when one
// arrives, replaces the bodies of both counters with their signatures
// unchanged; until then the rule is exact by definition and coarse only by
// implementation, and every fact priced with it says so through the capture
// method that labels its figures.
func CountDeliveredTokens(delivered []byte) int64 {
	return int64(len(delivered))
}
