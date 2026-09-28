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
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/analytics"
)

// maxTimezoneOctets is the bound the contract puts on the timezone parameter
// (shared maxLength 64). It is enforced here rather than trusted, because a
// parameter that is 4KB of "America/" nonsense is a request the resolver would
// spend real work on before refusing — and a refusal after the work is not a
// refusal the caller experiences as cheap.
const maxTimezoneOctets = 64

// usage is the one product endpoint this plane serves, and the handler is
// deliberately thin: it parses a query string, hands four values to the use
// case, and writes what comes back. Every decision worth arguing about is
// below that line — the bounds are the domain's, the scope is the use case's,
// and the status is the mapping's.
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
func usage(readModel *application.Usage) http.HandlerFunc {
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

		answer, err := readModel.Usage(r.Context(), token, request)
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
	usage analytics.Usage
}

// MarshalJSON writes the envelope the contract declares, field for field.
//
// It is hand-written because the domain's shape is not the wire shape: the
// domain carries time.Time and the contract carries RFC 3339 strings, the
// domain's balances are minor units and the contract's are the same integers
// with the currency's unit in the name, and the domain's availability is a
// string enum the contract spells out. A reflection-based marshaller would get
// all three approximately right, and approximately right is how a date-time
// ends up in local time and a _minor_units field ends up in major units.
func (response usageResponse) MarshalJSON() ([]byte, error) {
	answer := response.usage

	buckets := make([]usageBucket, 0, len(answer.Series))
	for _, point := range answer.Series {
		buckets = append(buckets, usageBucket{
			Start:                  point.Start.Format(time.RFC3339),
			End:                    point.End.Format(time.RFC3339),
			RequestsWithUsageFacts: point.WithUsageFacts,
			RequestsSettled:        point.Settled,
		})
	}

	return json.Marshal(usageEnvelope{
		Availability: string(answer.Availability),
		Metric:       "usage",
		Granularity:  string(answer.Range.Granularity),
		Range: usageRange{
			StartAt: answer.Range.From.Format(time.RFC3339),
			EndAt:   answer.Range.To.Format(time.RFC3339),
			// Already the canonical name: the domain's NewQuery resolved the
			// caller's spelling and stored the resolved location's name when
			// none was given, so the response echoes the zone the buckets were
			// cut in rather than re-deriving a name at the transport.
			Timezone: answer.Range.Timezone,
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
			DataThrough: answer.Freshness.DataThrough.Format(time.RFC3339),
			Basis:       string(answer.Freshness.Basis),
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
