package accounting

// CountDeliveredTokens is the gateway's count of what reached the client,
// taken over the bytes the reply recorded.
//
// It is deliberately a byte-length rule, stated here rather than guessed at
// each call site: until a canonical tokenizer exists (B11's grounding work),
// every settlement's delivery figure and every attempt's delivery count comes
// from this one function, so the number an auditor re-derives from a fact is
// the number this build published, whatever its precision. When the canonical
// count arrives it replaces the rule's body and every caller with it — the
// signature stays.
func CountDeliveredTokens(delivered []byte) int64 {
	return int64(len(delivered))
}
