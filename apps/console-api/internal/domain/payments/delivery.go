package payments

// DeliveryKey names ONE delivery, and it is the same triple the ledger's
// uniqueness is built on: the provider, the provider's identifier for the
// merchant account the delivery belongs to, and the provider's own event id.
//
// It exists as a type of its own because two different questions are asked of a
// recorded delivery and only one of them is answered by the record. "What did
// this delivery claim" is the row; "which row is this" is the key — and the
// second question is asked at a point where the row cannot be carried whole,
// because settling a delivery must not be able to rewrite what it claimed.
// Passing a ProviderEventRecord to that operation would put every column of the
// evidence in reach of a statement that has no business touching any of them.
//
// The merchant account is IN the key and is not a decoration; see
// ProviderEventRecord.ProviderAccountKey for why a provider that scopes its
// event ids per merchant would otherwise hand two customers the same id.
type DeliveryKey struct {
	Provider           string
	ProviderAccountKey string
	EventID            string
}

// DeliveryKey names the delivery a record describes.
//
// It is the record's identity and nothing else: the same three columns the
// schema's `payment_events_delivery_key` is declared over, in the same order.
// A method rather than a struct literal at each call site because the two must
// agree — a key built from the wrong column would settle a different delivery's
// verdict, which is a fact about somebody else's money.
func (r ProviderEventRecord) DeliveryKey() DeliveryKey {
	return DeliveryKey{
		Provider:           r.Provider,
		ProviderAccountKey: r.ProviderAccountKey,
		EventID:            r.EventID,
	}
}
