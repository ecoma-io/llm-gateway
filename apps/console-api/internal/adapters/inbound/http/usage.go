package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// maxTimezoneOctets is the bound the contract puts on the timezone parameter
// (shared maxLength 64). It is enforced here rather than trusted, because a
// parameter that is 4KB of "America/" nonsense is a request the resolver would
// spend real work on before refusing — and a refusal after the work is not a
// refusal the caller experiences as cheap.
const maxTimezoneOctets = 64

// UsageUseCases is the usage read's seam, and it is declared HERE rather than
// taken as the application's type for the reason ConsoleReadUseCases is: the
// import rule forbids this package from reaching internal/domain, so the seam
// speaks in the plain fields the wire itself needs (readwire_seam.go states the
// argument in full). The one difference between the two seams is what
// authenticates them — a read is a session cookie resolved before the call and
// an account passed as an argument, while this one is a CREDENTIAL handed over
// whole, because this surface is authenticated by a bearer token rather than by
// a session and nothing above this line is entitled to interpret one.
//
// It stays a separate interface rather than eleven more methods on
// ConsoleReadUseCases because the two are wired from different things — the ten
// reads take an account, this one resolves one — and a process whose credential
// resolution is missing is a different defect from a process whose screens are
// missing.
type UsageUseCases interface {
	// Usage answers one usage range for the account the presented token speaks
	// for. An unresolved credential is a refusal this method returns; the
	// handler does not interpret the token, and a caller that handed one over
	// and got an answer has learned nothing from this package about how it was
	// resolved.
	Usage(ctx context.Context, token string, request application.UsageRequest) (UsageAnswer, error)
}

// UsageAnswer is one usage answer in the transport's own vocabulary, and every
// field is plain: strings, integers, booleans and instants. No domain type
// crosses this boundary, which is what lets the composition root — the one
// place allowed to speak both grammars — do the rendering, and lets this
// package's tests drive the surface without naming a grammar at all.
//
// The instants are time.Time rather than pre-formatted strings because
// formatting is a WIRE decision and it belongs on this side of the seam: the
// envelope below writes them as RFC 3339 in UTC, and an answer type that
// arrived already formatted would have moved the transport's one formatting
// decision into the layer above it.
//
// The account is NOT here. It is not in the contract either, and a field that
// carried it would be the one field a test could pin — see usageResponse.
type UsageAnswer struct {
	// Availability is the contract's enum spelled as the contract spells it.
	Availability string
	// Granularity is the resolved grain, and it lives outside the range
	// because the contract puts it on the response: the same interval is a
	// hundred points at hourly grain and one at monthly.
	Granularity string
	From        time.Time
	To          time.Time
	// Timezone is the canonical name of the zone the buckets were cut in, so
	// the response echoes a zone rather than re-deriving a name here.
	Timezone string
	// FinalBucketPartial is true when the range's last bucket ends after the
	// figures are complete, so a client does not read a short bucket as a drop.
	FinalBucketPartial bool
	// Series is one entry per requested bucket, in order, with no bucket
	// omitted.
	Series []UsageAnswerBucket

	RequestsWithUsageFacts int64
	RequestsSettled        int64

	// The money axes, in the currency's minor units, as exact integers. They
	// are never netted against one another and no figure here is derived from
	// the series above.
	SettledMinorUnits    int64
	ReleasedMinorUnits   int64
	FundsAddedMinorUnits int64
	HeldMinorUnits       int64
	AvailableMinorUnits  int64

	// Capture splits the settlements that captured, by the method each used.
	Capture UsageAnswerCapture
	// Freshness says how current the figures are.
	Freshness UsageAnswerFreshness
}

// UsageAnswerBucket is one point of the series: the interval it covers and the
// two counts over it.
type UsageAnswerBucket struct {
	Start                  time.Time
	End                    time.Time
	RequestsWithUsageFacts int64
	RequestsSettled        int64
}

// UsageAnswerCapture is the three-way split of the settlements that captured.
type UsageAnswerCapture struct {
	Reported         int64
	GatewayObserved  int64
	ReservationFloor int64
}

// UsageAnswerFreshness is the answer's own currency: the instant the figures
// are complete through, and which pass established it.
type UsageAnswerFreshness struct {
	DataThrough time.Time
	Basis       string
}

// The handler is deliberately thin: it parses a query string, hands three
// values to the use case, and writes what comes back. Every decision worth
// arguing about is below that line — the bounds are the domain's, the scope is
// the use case's, and the status is the mapping's.
//
// What the handler does own, and what a thin handler is still responsible for:
//
//   - THE CREDENTIAL IS EXTRACTED, NOT INTERPRETED. The Authorization header
//     is read and the token handed over whole; nothing here decides what it
//     authenticates, because a handler that understood a credential would be a
//     second implementation of a policy the domain already owns.
//   - AN UNKNOWN QUERY PARAMETER IS A REFUSAL, not an ignored field. The
//     contract says so, and the reason is that a silently-ignored parameter is
//     indistinguishable from a misspelled one: a caller asking for
//     `group_by=model` would receive a correct answer about the wrong thing.
//   - THE ANSWER'S INSTANTS GO OUT IN UTC and the zone the buckets were cut in
//     travels as a field. A bucket edge in a local zone is what the caller
//     asked for, but a date-time with an offset in an envelope that stores
//     everything in UTC is a place two clients will disagree about.
func handleUsage(useCases UsageUseCases) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			// Refused here, before the use case, so a request with no
			// credential at all never reaches the read model and never
			// becomes a database round trip. It is the same refusal the
			// resolver would produce for a token that does not resolve, and
			// the caller cannot tell the two apart.
			writeError(w, r, application.UnresolvedCredential())
			return
		}

		query := r.URL.Query()
		if unexpected := unknownParameters(query, "from", "to", "granularity", "timezone"); len(unexpected) > 0 {
			writeError(w, r, application.InvalidRequest("unknown query parameter "+strings.Join(unexpected, ", ")+
				": this surface answers a fixed set of questions, and a parameter it ignored would be indistinguishable from a misspelled one"))
			return
		}

		request, err := usageRequest(query)
		if err != nil {
			writeError(w, r, err)
			return
		}

		answer, err := useCases.Usage(r.Context(), token, request)
		if err != nil {
			// A read that ran out of its budget is a 503 and not a 500: the
			// contract promises the caller a retry-later answer, and a 500
			// would read as a defect in this service rather than as a load
			// shape it would answer a moment later.
			if isDeadline(err) {
				writeError(w, r, readTimeoutError{})
				return
			}
			writeError(w, r, err)
			return
		}

		writeJSON(w, http.StatusOK, usageResponse{usage: answer})
	}
}

// bearerToken extracts the token from the Authorization header.
//
// The scheme is matched case-insensitively, as RFC 7235 requires, and both the
// absent header and a header this surface cannot read are the SAME refusal —
// a caller that is told which of the two it got has learned one bit about a
// credential space it did not have.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}

// usageRequest binds the four declared parameters, and refuses rather than
// assuming for every one of them.
//
// The instants are parsed as RFC 3339 because the contract declares them in
// that format. A date without a time, or a time without a zone, is a REFUSAL
// here rather than a coercion: "2026-09-01" is ambiguous by a whole day of
// offset depending on which end of it the reader assumed, and a report that
// silently resolved an ambiguity is a report whose buckets are in the wrong
// place with nothing to say so.
func usageRequest(query map[string][]string) (application.UsageRequest, error) {
	request := application.UsageRequest{}

	from, err := requiredInstant(query, "from")
	if err != nil {
		return application.UsageRequest{}, err
	}
	request.From = from

	to, err := requiredInstant(query, "to")
	if err != nil {
		return application.UsageRequest{}, err
	}
	request.To = to

	granularity, err := first(query, "granularity")
	if err != nil {
		return application.UsageRequest{}, err
	}
	if granularity == "" {
		return application.UsageRequest{}, application.InvalidRequest("granularity is required, and is one of hour, day or calendar_month")
	}
	request.Granularity = granularity

	// The one defaulted parameter: an absent timezone is UTC, which is where
	// every figure is stored, so naming nothing names the store.
	zone, err := first(query, "timezone")
	if err != nil {
		return application.UsageRequest{}, err
	}
	switch {
	case zone != "":
		if len(zone) > maxTimezoneOctets {
			return application.UsageRequest{}, application.InvalidRequest("the timezone is longer than the " +
				strconv.Itoa(maxTimezoneOctets) + " octets this surface accepts")
		}
		request.Timezone = zone
	default:
		if _, present := query["timezone"]; present {
			// `?timezone=` was a request for a zone and carried no name. That is
			// a question this surface cannot answer, and reading it as UTC would
			// answer it anyway — in a way a caller with a broken client would
			// never notice.
			return application.UsageRequest{}, application.InvalidRequest("the timezone was present and empty: a zone this surface cannot name is not the same as no zone, which is UTC")
		}
	}

	return request, nil
}

func requiredInstant(query map[string][]string, name string) (time.Time, error) {
	raw, err := first(query, name)
	if err != nil {
		return time.Time{}, err
	}
	if raw == "" {
		return time.Time{}, application.InvalidRequest(name + " is required, as an absolute RFC 3339 instant with an offset: " +
			"a bare date is ambiguous by a whole day of offset depending on which end of it the reader assumed")
	}
	instant, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, application.InvalidRequest(name + " is not an RFC 3339 instant: " + err.Error())
	}
	return instant, nil
}

// first returns the single value of a parameter, and REFUSES a parameter given
// twice.
//
// A parameter given twice is two different questions wearing one name. Answering
// for the first and reporting the second's silence would leave a caller with an
// answer it cannot tell was answered for the wrong value, which is a worse
// failure than a refusal: the chart renders, the numbers look real, and the
// caller's own request was never the one that produced them.
func first(query map[string][]string, name string) (string, error) {
	values, ok := query[name]
	if !ok || len(values) == 0 {
		return "", nil
	}
	if len(values) > 1 {
		return "", application.InvalidRequest(name + " was given " + strconv.Itoa(len(values)) +
			" times: this surface answers for one value, and picking one of them would be a report about a request the caller did not make")
	}
	return values[0], nil
}

// unknownParameters lists what the caller sent that this surface does not
// answer, sorted so the message is deterministic.
func unknownParameters(query map[string][]string, declared ...string) []string {
	known := make(map[string]struct{}, len(declared))
	for _, name := range declared {
		known[name] = struct{}{}
	}
	unexpected := make([]string, 0, len(query))
	for name := range query {
		if _, ok := known[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(unexpected)
	return unexpected
}

// isDeadline reports whether err is a deadline overrun, whether it reached the
// use case wrapped or arrived as the context's own error.
//
// Both are checked because the port can surface either: a driver that honours
// the context returns context.DeadlineExceeded, and one that does not returns
// its own "canceling statement due to statement timeout" — and the store
// translates the server-side one to its own sentinel, which the use case wraps
// in an Internal whose Unwrap chain is the only thing that carries the cause
// this reads.
func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// usageResponse is the wire envelope for one usage answer, and every field is
// named after what it holds rather than after where it came from.
//
// The account is NOT here. The contract does not declare it, and a response
// carrying the account would be a response a caller could assert against — the
// one field whose value a test could pin, which is exactly the field a
// cross-account test would want to tamper with if the surface ever grew an
// account parameter. The scope is proven by the numbers, not asserted by an
// echo.
type usageResponse struct {
	usage UsageAnswer
}

// MarshalJSON writes the envelope the contract declares, field for field.
//
// It is hand-written because the answer's shape is not the wire shape in three
// specific places: the answer carries time.Time and the contract carries RFC
// 3339 strings, the answer's balances are minor units and the contract's are
// the same integers with the currency's unit in the name, and the answer's
// series is one struct per bucket while the contract names each field. A
// reflection-based marshaller would get all three approximately right, and
// approximately right is how a date-time ends up in local time and a
// `_minor_units` field ends up in major units.
func (response usageResponse) MarshalJSON() ([]byte, error) {
	answer := response.usage

	buckets := make([]usageBucket, 0, len(answer.Series))
	for _, point := range answer.Series {
		buckets = append(buckets, usageBucket{
			Start:                  wireInstant(point.Start),
			End:                    wireInstant(point.End),
			RequestsWithUsageFacts: point.RequestsWithUsageFacts,
			RequestsSettled:        point.RequestsSettled,
		})
	}

	return json.Marshal(usageEnvelope{
		Availability: answer.Availability,
		Metric:       "usage",
		Granularity:  answer.Granularity,
		Range: usageRange{
			StartAt: wireInstant(answer.From),
			EndAt:   wireInstant(answer.To),
			// The zone the buckets were cut in, echoed as the answer named it:
			// resolving the caller's spelling is the use case's, and
			// re-deriving a name from the instants here is a second
			// implementation of that.
			Timezone: answer.Timezone,
		},
		FinalBucketPartial:     answer.FinalBucketPartial,
		Series:                 buckets,
		RequestsWithUsageFacts: answer.RequestsWithUsageFacts,
		RequestsSettled:        answer.RequestsSettled,
		SettledMinorUnits:      answer.SettledMinorUnits,
		ReleasedMinorUnits:     answer.ReleasedMinorUnits,
		FundsAddedMinorUnits:   answer.FundsAddedMinorUnits,
		HeldMinorUnits:         answer.HeldMinorUnits,
		AvailableMinorUnits:    answer.AvailableMinorUnits,
		Capture: usageCapture{
			Reported:         answer.Capture.Reported,
			GatewayObserved:  answer.Capture.GatewayObserved,
			ReservationFloor: answer.Capture.ReservationFloor,
		},
		Freshness: usageFreshness{
			DataThrough: wireInstant(answer.Freshness.DataThrough),
			Basis:       answer.Freshness.Basis,
		},
	})
}

type usageEnvelope struct {
	Availability       string        `json:"availability"`
	Metric             string        `json:"metric"`
	Range              usageRange    `json:"range"`
	Granularity        string        `json:"granularity"`
	FinalBucketPartial bool          `json:"final_bucket_partial"`
	Series             []usageBucket `json:"series"`

	RequestsWithUsageFacts int64 `json:"requests_with_usage_facts"`
	RequestsSettled        int64 `json:"requests_settled"`

	SettledMinorUnits    int64 `json:"settled_amount_minor_units"`
	ReleasedMinorUnits   int64 `json:"released_amount_minor_units"`
	FundsAddedMinorUnits int64 `json:"funds_added_minor_units"`
	HeldMinorUnits       int64 `json:"held_minor_units"`
	AvailableMinorUnits  int64 `json:"available_minor_units"`

	Capture   usageCapture   `json:"capture"`
	Freshness usageFreshness `json:"freshness"`
}

// wireInstant renders one instant the way this contract spells instants: RFC
// 3339, in UTC.
//
// The zone is stated rather than merely formatted, and that is the difference
// between a claim and a convention. `Format(time.RFC3339)` writes the offset the
// instant happens to carry, so a bucket edge that arrived in the zone the caller
// asked about would go out as `2026-09-01T05:30:00+05:30` — a correct instant
// that a caller comparing two responses from two zones could not compare, and
// that a client in a third zone would have to re-derive. Storage is UTC (the
// contract's `timezone` parameter says so), the zone travels as its own field,
// and this function is what makes both of those true of every response this
// surface writes rather than true by luck.
func wireInstant(instant time.Time) string {
	return instant.UTC().Format(time.RFC3339)
}

// usageRange is the range the answer covers, and it carries the GRAIN OUTSIDE
// itself. The contract puts `granularity` on the response rather than on the
// range: a range is an interval, and an interval has a width, but the grain is
// the width of one point in the series rather than a property of the interval
// — the same interval is a hundred points at hourly grain and one at monthly.
// A client built from the fragment reads the field where the fragment puts it,
// and a handler that nests it produces a document the client cannot parse.
type usageRange struct {
	StartAt  string `json:"start_at"`
	EndAt    string `json:"end_at"`
	Timezone string `json:"timezone"`
}

type usageBucket struct {
	Start                  string `json:"bucket_start"`
	End                    string `json:"bucket_end"`
	RequestsWithUsageFacts int64  `json:"requests_with_usage_facts"`
	RequestsSettled        int64  `json:"requests_settled"`
}

type usageCapture struct {
	Reported         int64 `json:"reported"`
	GatewayObserved  int64 `json:"gateway_observed"`
	ReservationFloor int64 `json:"reservation_floor"`
}

type usageFreshness struct {
	DataThrough string `json:"data_through"`
	Basis       string `json:"basis"`
}
