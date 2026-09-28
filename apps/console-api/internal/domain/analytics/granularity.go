package analytics

// Granularity is the width of one point in a series, and the only dimension a
// caller may choose. It is a closed enumeration rather than a duration because
// a duration would put the vocabulary in the caller's hands: "24h" and "1d" are
// the same width in one zone and different widths in another, and a caller
// choosing between them would be choosing between two different questions
// without being able to tell.
type Granularity string

const (
	// GranularityHour cuts the range into fixed one-hour buckets. It is the
	// only grain that needs no calendar from the display zone, and the only
	// one that is a fixed width everywhere: a zone's offset moves it and its
	// length never changes.
	GranularityHour Granularity = "hour"
	// GranularityDay cuts the range into calendar days IN THE DISPLAY ZONE.
	// Under a daylight-saving transition one of them is 23 or 25 hours
	// long, which is why every bucket carries both its start and its end
	// rather than a start and a nominal width.
	GranularityDay Granularity = "day"
	// GranularityCalendarMonth cuts the range into calendar months in the
	// display zone — February, not thirty days. A report mixing this with a
	// duration-based grain would be a report whose buckets cannot be
	// re-aggregated without re-deriving what a bucket meant.
	GranularityCalendarMonth Granularity = "calendar_month"
)

// knownGrains is the closed vocabulary, kept as a set so the parser and the
// maximum-buckets rule read the same three names the contract declares.
var knownGrains = map[Granularity]struct{}{
	GranularityHour:          {},
	GranularityDay:           {},
	GranularityCalendarMonth: {},
}

// ParseGranularity resolves a request's grain. The refusal is the point: a
// grain this build does not know is a request it cannot answer correctly, and
// defaulting it to the finest grain would answer a different question than
// the one asked.
func ParseGranularity(raw string) (Granularity, error) {
	granularity := Granularity(raw)
	if _, known := knownGrains[granularity]; !known {
		return "", errInvalidGranularity(raw)
	}
	return granularity, nil
}

// NeedsCalendar reports whether cutting this grain requires a calendar from the
// display zone. The hour grain does not: every hour is an hour long in every
// zone, so a fixed-offset zone cuts them correctly and only the day and month
// grains need zone rules to know where a calendar day begins.
func (g Granularity) NeedsCalendar() bool {
	return g != GranularityHour
}
