package analytics

import "errors"

// The domain's sentinel errors. Use cases and adapters match on these with
// errors.Is; everything a caller could branch on is here, and the constructors
// wrap them with the context that names the rule that was broken.
//
// Every one of these is a REFUSAL, and that is the point: a bound this domain
// enforces is answered by declining to produce an answer. A clamped range is
// indistinguishable from the range that was asked for, and a caller could not
// learn of the mismatch anywhere in the answer.
var (
	// ErrInvalidRange is a range that is not a non-empty half-open
	// interval: an end at or before its start. Half-open by construction,
	// because an inclusive upper bound would make the instant a range ends
	// at belong to two ranges at once.
	ErrInvalidRange = errors.New("invalid range")
	// ErrRangeTooLong is a range longer than MaxRange. The maximum is a
	// refusal rather than a truncation for the reason above, and it bounds
	// the work rather than the answer: an unbounded range is a scan whose
	// cost grows with the life of the deployment.
	ErrRangeTooLong = errors.New("range too long")
	// ErrSeriesTooLong is a range whose grain would cut it into more than
	// MaxSeriesPoints buckets. It is a separate refusal from ErrRangeTooLong
	// because at the finest grain the two coincide, and a caller that
	// shortened the range would learn from the wrong message that the
	// series was the problem when the range was.
	ErrSeriesTooLong = errors.New("series too long")
	// ErrInvalidGranularity is a grain outside the closed enumeration. The
	// vocabulary is fixed: a grain this build does not know is a request
	// this build cannot answer correctly, which is a different thing from a
	// request that happens to be large.
	ErrInvalidGranularity = errors.New("invalid granularity")
	// ErrInvalidTimezone is a display zone that does not resolve. It is
	// refused rather than defaulted, and the asymmetry with the
	// default-UTC `timezone` parameter is deliberate: a caller that named no
	// zone asked for UTC, and a caller that named a zone which does not
	// exist asked a question this plane cannot answer. Falling back would
	// answer it anyway, in the wrong zone, and a chart in the wrong zone is
	// wrong in a way no number defends.
	ErrInvalidTimezone = errors.New("invalid timezone")
	// ErrUnresolvable is an arithmetic step that could not advance the walk
	// over a zone's calendar. It exists because the bucket walk terminates
	// on a check rather than on a counter, and a check that cannot pass is
	// the only honest way for that walk to end.
	ErrUnresolvable = errors.New("bucket bounds do not advance")
)
