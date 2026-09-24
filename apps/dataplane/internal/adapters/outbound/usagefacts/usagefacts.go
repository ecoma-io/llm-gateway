// Package usagefacts is the production implementation of the runtime's fact
// reader — and today it is the production implementation of "there is no
// durable source yet".
//
// Every fact-feed property the port documents is a property of a store: an
// append sequence to order by, a retention window to expire a cursor against,
// a transaction that makes a fact durable before its position is readable. The
// Data Plane has no such store: `usage_events` arrives with the accounting
// schema, and building a reader against a table that does not exist is writing
// the same design twice, the second time from the wrong end.
//
// So this adapter refuses. It returns ErrSourceUnavailable from every read,
// which the management listener maps to a 500 and which no caller papers over.
// That is not a stub and it is not an apology for one: the alternative
// available at scaffold stage is an in-memory feed, and an in-memory feed is
// worse than a refusal in every way that matters. It would satisfy the whole
// suite and every consumer, and lose every fact the moment the process
// restarted — a fact-delivery system that silently loses facts is precisely
// the failure the pull-and-replay model was chosen to remove.
//
// The refusal is also the honest statement of where this project is. A caller
// that reaches this adapter learns that the Data Plane cannot yet say what
// happened, instead of learning it later from a count that is quietly wrong.
package usagefacts

import (
	"context"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// Reader implements usagefacts.Reader over a durable source this build does
// not have.
//
// It is a type rather than a bare function because the day a store exists this
// type holds it: the wiring in cmd/dataplane changes this constructor's
// arguments and not its shape, and the package that depends on the port sees
// no change at all. The field will arrive with the pool.
type Reader struct{}

// New returns the reader this build wires.
func New() *Reader {
	return &Reader{}
}

// Read implements usagefacts.Reader.
//
// It returns ErrSourceUnavailable for every request, including one with an
// empty cursor: "there is nothing here" and "there is nothing here yet" are
// the same answer to a consumer, and an empty page would instead tell it that
// this Data Plane has recorded nothing — a claim this build cannot support,
// and one a consumer would act on by advancing a position past facts that do
// exist somewhere.
func (reader *Reader) Read(_ context.Context, _ string, _ int) (usagefacts.Page, error) {
	return usagefacts.Page{}, usagefacts.ErrSourceUnavailable
}
