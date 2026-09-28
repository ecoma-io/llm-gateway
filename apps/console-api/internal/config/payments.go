package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultWebhookTolerance is how stale a signed provider delivery may be
	// when no deployment sets one. Five minutes is the provider's own
	// recommendation for its signing scheme (Stripe's documentation uses 300
	// seconds as its example tolerance), and matching the provider's number
	// rather than inventing a rounder one is the point: the window is a joint
	// decision between two clocks, and the party that stamps the timestamp is
	// the one whose documentation says how far apart they may drift.
	DefaultWebhookTolerance = 5 * time.Minute

	// MaxWebhookTolerance is the widest staleness this configuration accepts.
	// Fifteen minutes is three times the provider's own recommendation and is
	// deliberately not generous: the tolerance is the window in which a
	// captured delivery can be replayed, and nothing this platform does needs
	// a delivery older than the provider's own retry schedule — a stale
	// delivery is recorded and answered, and the window is what bounds how
	// much of the provider's history an attacker who captured one delivery can
	// still make this plane act on.
	MaxWebhookTolerance = 15 * time.Minute

	// DefaultPaymentsRequestTimeout bounds one outbound call to the provider
	// when no deployment sets one. Ten seconds is generous against a
	// hosted-checkout API — which answers in tens of milliseconds — and short
	// enough that a customer waiting on the redirect is not held behind a
	// provider that has stopped answering. The call sits on a browser-facing
	// request path, so this bound is what the customer's patience is spent
	// against, and there is no ambient deadline above it.
	DefaultPaymentsRequestTimeout = 10 * time.Second

	// maxPaymentsProviderLength bounds the provider name. It is the same
	// alphabet and the same ceiling the domain's own provider namespace folds
	// into (payments.foldProvider: lowercase letters, digits, hyphen,
	// underscore), so a name this configuration accepts is a name every
	// downstream use of it can carry.
	maxPaymentsProviderLength = 64

	// topUpOfferIDsVariable declares this deployment's top-up offers, and each
	// offer's own settings live in variables spelled from its id beside it. It
	// is the shape the Data Plane's egress configuration uses for a list of
	// members (DATAPLANE_EGRESS_POLICIES names the policies, and each one's TYPE
	// and ADDR sit beside it): the DECLARATION is one variable, and nothing is
	// positional, so there is no separator for a value to hide behind and no
	// order for two readings to disagree about.
	topUpOfferIDsVariable = "CONSOLE_API_PAYMENTS_TOP_UP_OFFER_IDS"

	// topUpOfferVariablePrefix is the prefix of every variable a declared offer
	// is spelled by: prefix + the offer id's environment spelling + the field.
	topUpOfferVariablePrefix = "CONSOLE_API_PAYMENTS_TOP_UP_OFFER_"

	// MaxTopUpOffers is the number of offers this group carries at most. A
	// ceiling rather than a policy about shops: an offer is a price an operator
	// publishes and a console renders, and a deployment declaring thousands of
	// them is a configuration that failed to render — the same judgement the
	// group's own all-or-nothing rule makes about a half-written group, applied
	// to a list that is technically complete. It also gives the label bound
	// below something to multiply against.
	MaxTopUpOffers = 64

	// maxTopUpOfferLabelLength bounds the human string a chooser renders. It is
	// a bound on SIZE rather than on shape, because a label is prose and is
	// shown to a customer as it was configured; what it stops is a configuration
	// value that is really a document.
	maxTopUpOfferLabelLength = 120

	// The exponent bounds are the domain's own, and they are stated here rather
	// than derived because this package may not import a domain package (see
	// internal/arch/imports_test.go: the domain is reachable from cmd,
	// application, adapters and ports, and configuration is not among them).
	// payments.validMinorUnitExponent accepts 0..4 — ISO 4217's exponents run
	// from 0 (JPY) to 4 (CLF) and nothing in the standard goes further — and
	// these two constants are that range, so a widening of one that misses the
	// other is a visible edit in two files rather than a silent divergence.
	minTopUpOfferExponent = 0
	maxTopUpOfferExponent = 4

	// MaxTopUpOfferAmountMinorUnits is the largest price this deployment may
	// put on the wire, and the bound is a fact about JSON rather than about
	// money: 2^53-1 is the largest integer a double-precision float represents
	// exactly, and every JavaScript client that reads this value — the console
	// is one, and the generated API client is TypeScript on top of one — parses
	// a JSON number into exactly that. A price above it does not fail anywhere;
	// it arrives as a DIFFERENT number, silently, and the customer is charged a
	// figure nobody wrote. int64 goes seven orders of magnitude further, so the
	// ceiling is real and reachable in principle, which is why it is a check
	// rather than a remark.
	//
	// The bound is applied at the OFFER, where a price enters this process, and
	// not to the payment that carries it: a payment's amount is copied from the
	// offer it names (see the application's openIntent), so one check covers
	// every figure that can reach a bucket.
	MaxTopUpOfferAmountMinorUnits = int64(1)<<53 - 1
)

// placeholderWebhookSecrets are the values a deployment must not ship as its
// webhook signing secret.
//
// The list is short and named rather than heuristic — a length rule would
// admit the very values below — and the reason it exists at all is the shape
// of the failure: an endpoint whose secret is the example value from the
// provider's documentation is an endpoint whose signature anyone who read that
// documentation can forge, and nothing about the running system looks wrong.
// The forgeries verify, the events are well-formed, and the money moves. A
// configuration refusal is the only place this defect is visible.
var placeholderWebhookSecrets = []string{"changeme", "secret", "test", "placeholder"}

// Payments holds this deployment's settings for the external payment provider:
// where it is, what this process presents to it, and the two bounds that govern
// one call and one delivery.
//
// It is a group of its own rather than four more fields on DataPlane because
// the peer is not the Data Plane: the provider is a third party reached over
// the public internet, holding money rather than state, and a deployment that
// points the console at a different processor changes every field here and
// none of the ones there.
//
// Both secrets are plain strings, and that is a decision made for a mechanical
// reason rather than a stylistic one. internal/arch/secrets_test.go refuses any
// struct field that nests the keymaterial package's Secret, because fmt reaches
// into unexported fields and prints their bytes under a formatting verb; the
// redaction contract lives in LogValue below, exactly as it does for
// DataPlane.Credential and Postgres.DSN. What the type system cannot enforce
// here is enforced by the one method every log line goes through.
//
// The group is OFF unless a deployment names it. Every variable below is
// optional in the sense that a process with none of them set starts with a zero
// Payments and no payment surface — but a deployment that sets ANY of them must
// set all of the required ones, because a half-written payment configuration is
// a deployment template that failed to render, and the whole group is refused
// rather than silently completed with defaults. That is this package's usual
// distinction between absence and emptiness, applied to a group instead of a
// variable: absence is a documented default, and a partial group is an error.
type Payments struct {
	// Provider names which payment provider adapter this deployment uses.
	Provider string

	// SecretKey is the provider's API secret used to call it. NEVER logged,
	// never returned, never put in the frontend bundle.
	SecretKey string

	// WebhookSigningSecret verifies inbound provider webhooks. NEVER logged.
	WebhookSigningSecret string

	// ProviderAccountKey is this deployment's own identifier for its merchant
	// account at the provider, sent as part of the webhook dedup key.
	ProviderAccountKey string

	// APIBaseURL is the provider's API base URL.
	APIBaseURL string

	// CheckoutReturnURL is where the provider sends the customer back. It is
	// built from configuration, NEVER from a request parameter.
	CheckoutReturnURL string

	// WebhookTolerance bounds how stale a signed delivery may be.
	WebhookTolerance time.Duration

	// RequestTimeout bounds one outbound provider call.
	RequestTimeout time.Duration

	// TopUpOffers is what this deployment sells, in the order it declared them.
	//
	// The offers live in CONFIGURATION rather than in this repository's source,
	// and the reason is what an offer IS: an amount is a price, a price is a
	// deployment's own decision about what it sells in the currency it settles
	// in, and a price compiled into a binary means changing what a customer is
	// charged requires a release — of a program that has nothing to do with the
	// decision. Price lists also differ between the deployments that run this
	// console (a trial, a region, a customer paying in another currency), and a
	// build-time constant would make each of those a build.
	//
	// It is a slice rather than a map, and the ordering is part of the value: a
	// chooser renders these in the order an operator wrote them, and a map would
	// hand that decision to Go's iteration order. The application keys its own
	// catalogue by id, where the only operation is a lookup.
	//
	// An EMPTY list is legal and means this deployment publishes no top-up
	// surface — a coherent deployment, and the reason the list is not required
	// with the rest of the group. A MALFORMED one is not: an offer missing a
	// currency is a deployment template that failed to render, and completing it
	// with a default would be this process inventing a price.
	TopUpOffers []TopUpOffer
}

// TopUpOffer is one thing an account may buy, as a deployment configured it.
//
// The fields are the application's TopUpOffer plus the two it does not carry,
// and the difference is worth stating because it is a design decision and not an
// oversight: the label is what a chooser renders and the id is what a client
// sends, and both belong to the surface that shows and accepts them rather than
// to the payment arithmetic. The three money fields are the ones that reach the
// ledger, and they are the ones validation here is strictest about.
type TopUpOffer struct {
	// ID is the offer's stable identifier: what a client names when it asks to
	// buy this offer, and what this offer's own variables are spelled from. It
	// must not change when the price does — a renamed offer is a broken client.
	ID string

	// AmountMinorUnits is the price in integer minor units, strictly positive.
	// It is an integer and never a decimal: the ledger stores this figure, the
	// provider is asked for exactly this figure, and the provider's event must
	// agree with it. A price written as "10.00" is refused rather than parsed,
	// because a spelling that needs a conversion is a price whose unit nobody
	// stated.
	AmountMinorUnits int64

	// Currency is the ISO 4217 code the price is denominated in, uppercase.
	Currency string

	// MinorUnitExponent is that currency's decimal places, 0..4.
	MinorUnitExponent int

	// Label is the human string a chooser renders.
	Label string
}

// LogValue renders the group safe for logs: the provider, its account
// identifier, the two URLs and the two bounds are visible; both secrets are
// not. Payments satisfies slog.LogValuer for the same reason DataPlane does —
// redaction by construction, not by call-site discipline — and the field names
// below are the configuration variable's own, so a log line and a deployment
// template read as the same thing.
//
// The account key is deliberately visible. It is an identifier rather than a
// credential: it names a merchant account, it appears in the webhook dedup key
// this process derives, and an operator reading a log line needs it to tell an
// event from the wrong account apart from an event from the wrong provider. The
// two fields that could be used to ACT — the API secret and the signing secret
// — are the two that are struck out.
func (p Payments) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("provider", p.Provider),
		slog.String("secret_key", "[redacted]"),
		slog.String("webhook_signing_secret", "[redacted]"),
		slog.String("provider_account_key", p.ProviderAccountKey),
		slog.String("api_base_url", p.APIBaseURL),
		slog.String("checkout_return_url", p.CheckoutReturnURL),
		slog.Duration("webhook_tolerance", p.WebhookTolerance),
		slog.Duration("request_timeout", p.RequestTimeout),
		// The offers are logged as the declaration an operator wrote: which
		// offers exist, in the order they were declared. The id is an identifier
		// and not a credential, and it is the one field of an offer that names it
		// without quoting a price or a label — a log line is not a shop window,
		// and the amounts and labels are readable from the configuration itself.
		slog.String("top_up_offer_ids", strings.Join(topUpOfferIDs(p.TopUpOffers), ",")),
	)
}

// String and GoString are the fmt half of the redaction, and they exist because
// LogValue only covers the path that goes through slog.
//
// A `%v`, `%s`, `%+v` or `%#v` on this value — in a startup line that predates
// the logger, in a `t.Fatalf`, in a panic message, in whatever a future error
// path reaches for — never touches LogValue at all, and Go prints every field
// of a struct that has no String method. Both verbs are implemented because fmt
// calls GoString for %#v and String for the rest, and implementing only one
// leaves the other printing the secrets; both return the same text, which is
// the text LogValue builds, so there is one rendering of this group and no
// second place for a new field to be forgotten.
//
// It is a value receiver, so it holds for the value, a pointer to it, and every
// copy a caller makes on the way to a format verb.
func (p Payments) String() string {
	return "config.Payments{provider:" + p.Provider +
		" secret_key:[redacted] webhook_signing_secret:[redacted]" +
		" provider_account_key:" + p.ProviderAccountKey +
		" api_base_url:" + p.APIBaseURL +
		" checkout_return_url:" + p.CheckoutReturnURL +
		" webhook_tolerance:" + p.WebhookTolerance.String() +
		" request_timeout:" + p.RequestTimeout.String() +
		" top_up_offer_ids:[" + strings.Join(topUpOfferIDs(p.TopUpOffers), ",") + "]}"
}

// GoString renders the same text for %#v. See String.
func (p Payments) GoString() string { return p.String() }

// paymentsVariables is every variable this group reads, in one place.
//
// It is a list rather than six separate probes because the loader has to answer
// one question before it reads any of them: did this deployment say anything
// about payments at all? A group that is off must stay off, and probing for
// presence variable by variable is how a deployment that set only a timeout
// ends up silently running no payments instead of failing loudly.
var paymentsVariables = []string{
	"CONSOLE_API_PAYMENTS_PROVIDER",
	"CONSOLE_API_PAYMENTS_SECRET_KEY",
	"CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET",
	"CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY",
	"CONSOLE_API_PAYMENTS_API_BASE_URL",
	"CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL",
	"CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE",
	"CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT",
	// The offers' own variables are spelled from the ids the list declares, so
	// they cannot appear here: this list is what answers "did this deployment
	// say anything about payments at all", and the declaration is the one
	// variable whose name is fixed. A deployment that spelled an offer's fields
	// without declaring the offer has set variables nothing reads — the same
	// edge the Data Plane's per-policy variables have, and it exists because
	// LookupEnv can ask about a name and cannot enumerate the environment.
	topUpOfferIDsVariable,
}

// loadPayments reads the CONSOLE_API_PAYMENTS_* variables and validates the
// whole group before returning it.
//
// The first thing it does is decide whether the group is configured at all. If
// not one of the variables is set, the zero Payments is returned and the
// process starts without a payment surface: this plane's own doctrine is that
// it can be down for a week without the inference path noticing, and a control
// plane that refused to start because a feature it was not asked to run had no
// credentials would be a console taken down by an absent setting.
//
// The moment one variable IS set the group is all-or-nothing. The provider, the
// API secret, the signing secret, the account key and the two URLs are required
// — there is no default for who the provider is, and a default credential would
// be a credential printed in this file — while the two bounds default, because
// a tolerance and a timeout nobody tuned are survivable and a missing signing
// secret is not.
func loadPayments(lookup LookupEnv) (Payments, error) {
	configured := false
	for _, name := range paymentsVariables {
		if _, ok := lookup(name); ok {
			configured = true
			break
		}
	}
	if !configured {
		return Payments{}, nil
	}

	provider, ok := lookup("CONSOLE_API_PAYMENTS_PROVIDER")
	if !ok {
		return Payments{}, fmt.Errorf("CONSOLE_API_PAYMENTS_PROVIDER must be set: the payment settings are configured as a group, and a deployment that sets any of them names the provider that will take the money")
	}
	cfg := Payments{Provider: provider}

	secretKey, ok := lookup("CONSOLE_API_PAYMENTS_SECRET_KEY")
	if !ok {
		return Payments{}, fmt.Errorf("CONSOLE_API_PAYMENTS_SECRET_KEY must be set: the provider authenticates this process, and a checkout that cannot authenticate never opens")
	}
	if secretKey == "" {
		return Payments{}, fmt.Errorf("CONSOLE_API_PAYMENTS_SECRET_KEY must not be empty")
	}
	cfg.SecretKey = secretKey

	signingSecret, ok := lookup("CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET")
	if !ok {
		return Payments{}, fmt.Errorf("CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET must be set: without it this process cannot tell its provider's deliveries from anyone else's")
	}
	if signingSecret == "" {
		return Payments{}, fmt.Errorf("CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET must not be empty")
	}
	cfg.WebhookSigningSecret = signingSecret

	accountKey, ok := lookup("CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY")
	if !ok {
		return Payments{}, fmt.Errorf("CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY must be set: it is part of the webhook dedup key, and an empty one is a key every deployment would share")
	}
	if accountKey == "" {
		return Payments{}, fmt.Errorf("CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY must not be empty")
	}
	cfg.ProviderAccountKey = accountKey

	apiBaseURL, ok := lookup("CONSOLE_API_PAYMENTS_API_BASE_URL")
	if !ok {
		return Payments{}, fmt.Errorf("CONSOLE_API_PAYMENTS_API_BASE_URL must be set: this process has no default for where the provider's API is")
	}
	cfg.APIBaseURL = apiBaseURL

	returnURL, ok := lookup("CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL")
	if !ok {
		return Payments{}, fmt.Errorf("CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL must be set: it is where every customer lands after checkout, and this process builds it from nothing a request can name")
	}
	cfg.CheckoutReturnURL = returnURL

	cfg.WebhookTolerance = DefaultWebhookTolerance
	if value, ok := lookup("CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE"); ok {
		duration, err := parsePositiveDuration("CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE", value)
		if err != nil {
			return Payments{}, err
		}
		cfg.WebhookTolerance = duration
	}

	cfg.RequestTimeout = DefaultPaymentsRequestTimeout
	if value, ok := lookup("CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT"); ok {
		duration, err := parsePositiveDuration("CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT", value)
		if err != nil {
			return Payments{}, err
		}
		cfg.RequestTimeout = duration
	}

	offers, err := loadTopUpOffers(lookup)
	if err != nil {
		return Payments{}, err
	}
	cfg.TopUpOffers = offers

	if err := validatePayments(cfg); err != nil {
		return Payments{}, err
	}
	return cfg, nil
}

// validatePayments refuses the shapes that look like a working payment
// integration and are not.
//
// Each rule below is written where it is because the failure it prevents is
// silent: a URL that carries credentials is logged by code that has no idea it
// is holding a secret, a placeholder signing secret verifies forgeries, a stale
// window hours wide keeps a captured delivery replayable, and a provider name
// outside the grammar reaches a namespace that folds it into a different one.
// None of those produce an error at runtime; every one of them is a divergence
// between what the deployment believes it configured and what it did.
//
// The offers are validated in their own function and called from here, because
// their refusals are the ones that need the WHOLE list in hand: everything an
// individual offer can get wrong is refused where it is read, with the variable
// an operator has to edit named in the message.
func validatePayments(cfg Payments) error {
	if cfg.Provider == "" {
		return fmt.Errorf("CONSOLE_API_PAYMENTS_PROVIDER must not be empty")
	}
	if len(cfg.Provider) > maxPaymentsProviderLength || !validPaymentsProvider(cfg.Provider) {
		// The value is echoed here and nowhere else in this file, and it is
		// echoed deliberately: a provider name is not a secret, and an operator
		// who typed "Stripe" needs to see what was refused to know that the
		// grammar wants the lowercase form.
		return fmt.Errorf("CONSOLE_API_PAYMENTS_PROVIDER %q must be 1 to %d characters of lowercase letters, digits, hyphens and underscores: it becomes the namespace segment of this provider's ledger command keys, and the domain folds anything outside that alphabet into a different name", cfg.Provider, maxPaymentsProviderLength)
	}

	if cfg.SecretKey == "" {
		return fmt.Errorf("CONSOLE_API_PAYMENTS_SECRET_KEY must not be empty")
	}

	if cfg.WebhookSigningSecret == "" {
		return fmt.Errorf("CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET must not be empty")
	}
	if isPlaceholderWebhookSecret(cfg.WebhookSigningSecret) {
		// The failure this refuses has no symptom: the webhook endpoint keeps
		// answering, the signature keeps verifying, and every forged delivery
		// signed with the provider's own documented example value is accepted
		// as ours. It looks exactly like a working integration, which is why
		// it is refused at startup rather than reviewed for later.
		return fmt.Errorf("CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET is one of the documented example values (%s): a deployment that ships it has a webhook endpoint whose signature anyone who read the provider's documentation can forge, and nothing about the running system would look wrong", strings.Join(placeholderWebhookSecrets, ", "))
	}

	if cfg.ProviderAccountKey == "" {
		return fmt.Errorf("CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY must not be empty")
	}

	if err := validatePaymentsURL("CONSOLE_API_PAYMENTS_API_BASE_URL", cfg.APIBaseURL); err != nil {
		return err
	}
	if err := validatePaymentsURL("CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL", cfg.CheckoutReturnURL); err != nil {
		return err
	}

	if cfg.WebhookTolerance <= 0 {
		return fmt.Errorf("CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE must be greater than zero: the tolerance is what bounds how long a captured delivery stays replayable, and a window of nothing is a window a provider's clock cannot be inside")
	}
	if cfg.WebhookTolerance > MaxWebhookTolerance {
		return fmt.Errorf("CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE (%s) must be at most %s: a tolerance of hours widens the window in which a captured delivery can be replayed, and buys nothing a provider's own retry schedule needs", cfg.WebhookTolerance, MaxWebhookTolerance)
	}

	if cfg.RequestTimeout <= 0 {
		return fmt.Errorf("CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT must be greater than zero: this call sits on a browser-facing request path with no deadline above it, so the caller's own bound is the only one there is")
	}

	if err := validateTopUpOffers(cfg.TopUpOffers); err != nil {
		return err
	}
	return nil
}

// validatePaymentsURL accepts an absolute http(s) URL naming a host, with no
// userinfo, no query and no fragment.
//
// It is validateDataPlaneURL's shape plus one check that function does not make
// and does not need to: userinfo. A management URL carrying embedded
// credentials is a mistake, but a PAYMENT URL carrying them is a secret in a
// configuration value that the non-secret code paths handle — it is logged by
// LogValue, it travels to the adapter, and it ends up in whatever an error
// path or a startup line decides to print. Refusing it here, where the failure
// is one startup line naming a variable, is the difference between a deployment
// that will not start and a deployment that will not start AND has printed its
// API credential.
//
// The query and the fragment are refused for the reason the dataplane's
// validator gives: both would be silently merged into, or dropped from, every
// request the outbound adapter builds, so a configuration value that part of a
// request ignores is a value that lies about what it does. Both URLs are held
// to one shape rather than one each, so this file has a single rule to read.
//
// Like validateDataPlaneURL and validatePostgresDSN, the value is never echoed
// in a failure: url.Parse quotes the string it rejected, credentials included,
// so its own text is deliberately not wrapped into these errors.
func validatePaymentsURL(name, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must use the http or https scheme", name)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s must name a host", name)
	}
	if parsed.User != nil {
		return fmt.Errorf("%s must not carry userinfo: a URL with embedded credentials puts a secret into a value that is logged and passed around by code with no idea it is holding one", name)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must carry no query or fragment", name)
	}
	return nil
}

// validPaymentsProvider reports whether name is inside the alphabet this
// deployment and the domain share: lowercase letters, digits, hyphens and
// underscores.
//
// It is the domain's foldProvider set — the characters that survive its fold
// unchanged — stated as an allow-list here because a configuration value is
// chosen once by an operator rather than received from a provider. The
// asymmetry with foldProvider, which maps everything else to an underscore
// rather than refusing, is deliberate: folding exists so a provider's own
// spelling cannot produce two namespaces, while a deployment writing its
// provider name has one correct spelling and every reason to be told what it
// is. An uppercase name is refused rather than folded so that the value in the
// environment, the value in the log line and the value in the command key are
// the same string.
func validPaymentsProvider(name string) bool {
	for _, character := range []byte(name) {
		switch {
		case character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// isPlaceholderWebhookSecret reports whether secret is one of the documented
// example values, ignoring case.
//
// Case is folded because "Test" is the same mistake as "test" and an operator
// who capitalises a placeholder is not making a more considered choice. The
// list stays a list of exact strings rather than a heuristic — a length rule,
// a pattern, an entropy estimate — because a heuristic would eventually refuse a
// genuine secret on a bad day, and the values below are the ones the provider's
// own documentation puts in front of a reader.
func isPlaceholderWebhookSecret(secret string) bool {
	for _, placeholder := range placeholderWebhookSecrets {
		if strings.EqualFold(secret, placeholder) {
			return true
		}
	}
	return false
}

// loadTopUpOffers reads the offers CONSOLE_API_PAYMENTS_TOP_UP_OFFER_IDS
// declares, in the order it declares them.
//
// The list variable is the DECLARATION and each offer's settings are four
// variables spelled from its own id, which is the shape the Data Plane's egress
// configuration already uses for a list of members. Nothing here is positional
// and nothing is separated by a delimiter, so there is no value that could be
// read two ways: an offer's amount is a variable with a name, not the third
// field of a record whose second field was a currency. That property is what a
// price needs — a value that parsed two ways would be a price that meant two
// things — and it is also why the ids cannot be read out of the variable names
// instead: the declaration names them, and this function follows it.
//
// An absent list is not an error. A deployment that sells nothing declares
// nothing and publishes no top-up surface, which is a coherent deployment and
// exactly the absence rule the whole group is built on. A list that IS present
// must be readable and every name on it must have all four of its settings,
// because a half-written offer is a deployment template that failed to render
// rather than an offer to complete with a default.
func loadTopUpOffers(lookup LookupEnv) ([]TopUpOffer, error) {
	raw, ok := lookup(topUpOfferIDsVariable)
	if !ok {
		return nil, nil
	}
	ids, err := parseTopUpOfferIDs(raw)
	if err != nil {
		return nil, err
	}

	offers := make([]TopUpOffer, 0, len(ids))
	for _, id := range ids {
		offer, err := readTopUpOffer(lookup, id)
		if err != nil {
			return nil, err
		}
		offers = append(offers, offer)
	}
	return offers, nil
}

// parseTopUpOfferIDs splits the declared list into the ids it names, and refuses
// every way one of them could not be configured.
//
// The alphabet is the group's own provider alphabet — lowercase letters, digits,
// hyphens and underscores — and the reason is not symmetry but the environment:
// an id's variables are spelled from it, so an id outside that alphabet could
// not be configured at all, and an id that folded into a spelling another id
// already claimed would silently read the other one's prices. The second case is
// refused rather than resolved, because there is no reading of two ids that
// share one set of variables that is more correct than the other.
func parseTopUpOfferIDs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%s must not be empty; omit the variable to publish no top-up offers", topUpOfferIDsVariable)
	}

	ids := []string{}
	claimants := map[string]string{}
	for _, element := range strings.Split(raw, ",") {
		id := strings.TrimSpace(element)
		if id == "" {
			return nil, fmt.Errorf("%s must not carry an empty offer id", topUpOfferIDsVariable)
		}
		if len(id) > maxPaymentsProviderLength || !validPaymentsProvider(id) {
			// The value is echoed, and echoing it is safe: an offer id is what a
			// client sends and what an operator wrote, never a secret.
			return nil, fmt.Errorf("%s: offer id %q must be 1 to %d characters of lowercase letters, digits, hyphens and underscores, because this offer's own variables are spelled from it", topUpOfferIDsVariable, id, maxPaymentsProviderLength)
		}
		segment := topUpOfferSegment(id)
		if first, ok := claimants[segment]; ok {
			return nil, fmt.Errorf("%s names both %q and %q, whose settings are both spelled %s: an environment cannot tell two offers apart by a name it spells the same way, and picking one reading would give a customer the other one's price", topUpOfferIDsVariable, first, id, segment)
		}
		claimants[segment] = id
		ids = append(ids, id)
	}
	return ids, nil
}

// topUpOfferSegment is an offer id's environment spelling: the id uppercased,
// with each hyphen written as the underscore an environment variable needs.
//
// The id itself is never recovered from the spelling — it comes from the
// declaration — so the transform only has to be a KEY, and the one property a
// key needs here is that two ids never share one. That is what parseTopUpOfferIDs
// refuses, and it is why this mapping is acceptable where a reverse one would
// not be.
func topUpOfferSegment(id string) string {
	return strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
}

// topUpOfferVariable is one offer field's variable name.
func topUpOfferVariable(segment, field string) string {
	return topUpOfferVariablePrefix + segment + "_" + field
}

// readTopUpOffer reads the four variables one declared offer is spelled by.
//
// Every field is required and none has a default, which is the opposite of the
// group's own bounds and for the same reason: a tolerance nobody set is
// survivable, while a currency nobody set is a price in no particular money and
// a label nobody set is a row in a chooser that renders nothing. Each refusal
// names the variable an operator has to set and the offer it belongs to.
func readTopUpOffer(lookup LookupEnv, id string) (TopUpOffer, error) {
	segment := topUpOfferSegment(id)
	offer := TopUpOffer{ID: id}

	amountVariable := topUpOfferVariable(segment, "AMOUNT_MINOR_UNITS")
	rawAmount, ok := lookup(amountVariable)
	if !ok {
		return TopUpOffer{}, fmt.Errorf("%s is missing: %s declares offer %q, and an offer with no amount is not a price", amountVariable, topUpOfferIDsVariable, id)
	}
	amount, err := parseTopUpOfferAmount(amountVariable, id, rawAmount)
	if err != nil {
		return TopUpOffer{}, err
	}
	offer.AmountMinorUnits = amount

	currencyVariable := topUpOfferVariable(segment, "CURRENCY")
	currency, ok := lookup(currencyVariable)
	if !ok {
		return TopUpOffer{}, fmt.Errorf("%s is missing: %s declares offer %q, and an amount is not a figure until the unit it counts in is known", currencyVariable, topUpOfferIDsVariable, id)
	}
	if !validPaymentsCurrency(currency) {
		return TopUpOffer{}, fmt.Errorf("%s: offer %q states the currency %q, which must be three uppercase letters of an ISO 4217 code", currencyVariable, id, currency)
	}
	offer.Currency = currency

	exponentVariable := topUpOfferVariable(segment, "MINOR_UNIT_EXPONENT")
	rawExponent, ok := lookup(exponentVariable)
	if !ok {
		return TopUpOffer{}, fmt.Errorf("%s is missing: %s declares offer %q, and the exponent is the difference between a thousand yen and ten dollars", exponentVariable, topUpOfferIDsVariable, id)
	}
	exponent, err := parseNonNegativeInt(exponentVariable, rawExponent)
	if err != nil {
		return TopUpOffer{}, err
	}
	if !validPaymentsExponent(exponent) {
		return TopUpOffer{}, fmt.Errorf("%s: offer %q states the exponent %d, and this build places a decimal point at %d to %d places", exponentVariable, id, exponent, minTopUpOfferExponent, maxTopUpOfferExponent)
	}
	offer.MinorUnitExponent = exponent

	labelVariable := topUpOfferVariable(segment, "LABEL")
	label, ok := lookup(labelVariable)
	if !ok {
		return TopUpOffer{}, fmt.Errorf("%s is missing: %s declares offer %q, and a chooser cannot render an offer that names nothing", labelVariable, topUpOfferIDsVariable, id)
	}
	if strings.TrimSpace(label) == "" {
		return TopUpOffer{}, fmt.Errorf("%s: offer %q has an empty label, so a chooser would render a row that says nothing", labelVariable, id)
	}
	if len(label) > maxTopUpOfferLabelLength {
		return TopUpOffer{}, fmt.Errorf("%s: offer %q has a label of %d characters, over the %d this build carries", labelVariable, id, len(label), maxTopUpOfferLabelLength)
	}
	offer.Label = label

	return offer, nil
}

// parseTopUpOfferAmount reads an offer's price in integer minor units.
//
// It is an int64 parse rather than the int parse the pool bounds use, because a
// price is a money figure and the ledger's own type is int64; and it is a parse
// of the WHOLE value rather than a scan of its prefix, so "10.00" and "10usd"
// are refused rather than read as ten. A price that needed a conversion would be
// a price whose unit nobody stated, and this is the one figure in the group that
// a customer is charged.
//
// The upper bound is the wire's, not the ledger's. MaxTopUpOfferAmountMinorUnits
// is the largest integer a JSON number carries through a JavaScript client
// exactly, and a price above it would not fail: it would arrive at the console
// as a different figure, with no error on either side. Refusing it here is the
// only place the mistake is visible, and it is visible while the operator is
// still editing the deployment template.
func parseTopUpOfferAmount(name, id, value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("%s must not be empty: an empty amount is not a price of nothing, it is a price nobody stated", name)
	}
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: offer %q must be an integer number of minor units, and %q is not one", name, id, value)
	}
	if amount <= 0 {
		return 0, fmt.Errorf("%s: offer %q must be greater than zero, and %d is not an offer", name, id, amount)
	}
	if amount > MaxTopUpOfferAmountMinorUnits {
		return 0, fmt.Errorf("%s: offer %q is priced at %d minor units, over the %d this build can carry to a client: JSON numbers above 2^53-1 lose precision in the console's own parser, so a price there would reach a customer as a different figure with nothing anywhere reporting a problem",
			name, id, amount, MaxTopUpOfferAmountMinorUnits)
	}
	return amount, nil
}

// validPaymentsCurrency reports whether code is an ISO 4217 alphabetic code.
//
// It is the domain's validCurrencyCode stated again, and the duplication is
// forced by the dependency rule rather than chosen: configuration may not import
// a domain package (internal/arch/imports_test.go names the packages that may,
// and this is not one of them), so the two cannot share a function. What they
// CAN share is the rule, and it is quoted here so that a widening of one is
// visible against the other: three characters, all of them uppercase A to Z. The
// check is a SHAPE and not membership of a list of currencies, for the reason
// the domain gives — a list would be a copy the world moves past, and a currency
// this build has never heard of is a payment it should still be able to carry.
func validPaymentsCurrency(code string) bool {
	if len(code) != 3 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return false
		}
	}
	return true
}

// validPaymentsExponent reports whether n is a decimal-place count this build
// places a point at.
//
// It is the domain's validMinorUnitExponent restated for the same reason
// validPaymentsCurrency is, against the bounds in the const block above, which
// are that function's 0..4. The bound is on PRECISION and not on currency
// membership: ISO 4217's exponents run from 0 to 4, a build that meets a larger
// one is looking at something that is not a currency amount, and a negative one
// is a value smaller than the unit it claims to be denominated in.
func validPaymentsExponent(n int) bool {
	return n >= minTopUpOfferExponent && n <= maxTopUpOfferExponent
}

// validateTopUpOffers refuses a catalogue this deployment could not render.
//
// The per-offer rules are enforced where each offer is read, because their
// refusals have to name the variable an operator edits; what is left for a
// function holding the whole list is the one rule that is about the list: its
// size. A deployment declaring more offers than the ceiling is a configuration
// that failed to render — an export that concatenated two environments, a
// template that looped over the wrong collection — and the console renders every
// offer it is given, so the failure would arrive as a chooser nobody can use
// rather than as an error anybody can read.
func validateTopUpOffers(offers []TopUpOffer) error {
	if len(offers) > MaxTopUpOffers {
		return fmt.Errorf("%s declares %d top-up offers, and this build carries at most %d: a list past the ceiling is a configuration that failed to render rather than a deployment selling more things", topUpOfferIDsVariable, len(offers), MaxTopUpOffers)
	}
	return nil
}

// topUpOfferIDs is the ids of a catalogue, in order, for a log line.
func topUpOfferIDs(offers []TopUpOffer) []string {
	ids := make([]string, 0, len(offers))
	for _, offer := range offers {
		ids = append(ids, offer.ID)
	}
	return ids
}
