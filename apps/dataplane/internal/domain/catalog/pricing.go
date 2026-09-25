package catalog

// PriceSnapshot is the price basis the effective revision supplies for one
// alias: the revision it was read from, that revision's operator-facing
// version, and the two unit prices in integer minor units per 1M tokens.
//
// It is a copy, not a live reference: admission freezes it into the request
// and its reservation, and every usage fact the request settles from carries
// the revision id and the prices, so a price list edited afterwards rewrites
// nothing that already happened. It is therefore the catalog-domain twin of
// execution.PriceSnapshot — same numbers, different owner: this type is what
// the read returns, that one is what a final request carries on its row.
type PriceSnapshot struct {
	// RevisionID is the client price list revision the prices came from, as
	// it travels in usage facts and reservations.
	RevisionID string
	// Version is that revision's operator-facing ordinal — the human-legible
	// twin of the id, so an operator can read a fact's price basis without a
	// join.
	Version int
	// InputUnitPrice prices prompt input, in integer minor units per 1M
	// tokens. Zero is a legitimate price.
	InputUnitPrice int64
	// OutputUnitPrice prices generated output, same unit and rules as
	// InputUnitPrice.
	OutputUnitPrice int64
}
