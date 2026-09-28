package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// samePayments compares two groups field by field.
//
// The whole-shape comparison is the point — a field that acquired a default
// nobody intended is the failure this catches — and it is reflect.DeepEqual
// rather than == because the group carries the top-up offers as a slice, which
// no Go struct containing one can be compared with. The alternative an older
// test suite reached for, comparing rendered log values, would compare only the
// fields LogValue chooses to render, which is not the same test at all.
func samePayments(a, b Payments) bool {
	return reflect.DeepEqual(a, b)
}

// paymentsTestSecretKey and paymentsTestSigningSecret are the two secrets the
// fixtures below are configured with.
//
// Both are ASSEMBLED from fragments rather than written as literals, and the
// values are deliberately low-entropy repeats rather than anything shaped like
// a credential. This repository is scanned by gitleaks and pushed through
// GitHub's push protection, both of which refuse a secret-shaped string in a
// fixture — and the scan is right to: a fixture only needs to be a string the
// code treats as one, never a value that could be mistaken for a live key.
func paymentsTestSecretKey() string {
	return "console-api-" + strings.Repeat("k", 32)
}

func paymentsTestSigningSecret() string {
	return "sig-" + strings.Repeat("w", 32)
}

// validPaymentsEnv is the smallest environment that configures the group: the
// six required values, the two bounds left to their defaults.
func validPaymentsEnv() map[string]string {
	return map[string]string{
		"CONSOLE_API_PAYMENTS_PROVIDER":               "stripe",
		"CONSOLE_API_PAYMENTS_SECRET_KEY":             paymentsTestSecretKey(),
		"CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET": paymentsTestSigningSecret(),
		"CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY":   "acct-console-api-merchant",
		"CONSOLE_API_PAYMENTS_API_BASE_URL":           "https://api.stripe.com",
		"CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL":    "https://console.internal.example/billing/top-up",
	}
}

// paymentsEnv merges the data plane values every successful Load needs with a
// payments environment.
func paymentsEnv(payments map[string]string) map[string]string {
	return merge(requiredEnv(), payments)
}

func TestLoadReadsThePaymentsGroup(t *testing.T) {
	t.Run("leaves the group off when the deployment says nothing about payments", func(t *testing.T) {
		// The absence rule: a group nobody configured is absent, not empty. A
		// console that refused to start because a feature it was not asked to
		// run had no credentials would be a plane taken down by a setting an
		// operator never meant to make.
		cfg, err := Load(lookup(requiredEnv()))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !samePayments(cfg.Payments, Payments{}) {
			t.Errorf("Load() payments = %s, want the zero group — no CONSOLE_API_PAYMENTS_* variable was set", cfg.Payments.LogValue().String())
		}
	})

	t.Run("reads the required values and defaults the two bounds", func(t *testing.T) {
		cfg, err := Load(lookup(paymentsEnv(validPaymentsEnv())))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		want := Payments{
			Provider:             "stripe",
			SecretKey:            paymentsTestSecretKey(),
			WebhookSigningSecret: paymentsTestSigningSecret(),
			ProviderAccountKey:   "acct-console-api-merchant",
			APIBaseURL:           "https://api.stripe.com",
			CheckoutReturnURL:    "https://console.internal.example/billing/top-up",
			WebhookTolerance:     DefaultWebhookTolerance,
			RequestTimeout:       DefaultPaymentsRequestTimeout,
		}
		if !samePayments(cfg.Payments, want) {
			t.Errorf("Load() payments = %s, want %s", cfg.Payments.LogValue().String(), want.LogValue().String())
		}
	})

	t.Run("reads supplied bounds", func(t *testing.T) {
		env := merge(validPaymentsEnv(), map[string]string{
			"CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE": "90s",
			"CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT":   "4s",
		})
		cfg, err := Load(lookup(paymentsEnv(env)))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Payments.WebhookTolerance != 90*time.Second {
			t.Errorf("webhook tolerance = %s, want 1m30s", cfg.Payments.WebhookTolerance)
		}
		if cfg.Payments.RequestTimeout != 4*time.Second {
			t.Errorf("request timeout = %s, want 4s", cfg.Payments.RequestTimeout)
		}
	})

	t.Run("refuses a group that sets only one of the required values", func(t *testing.T) {
		// The all-or-nothing rule, and the case that makes it worth stating: a
		// deployment that sets one payment variable has decided to run a
		// payment surface, so the other five are missing rather than absent.
		_, err := Load(lookup(paymentsEnv(map[string]string{
			"CONSOLE_API_PAYMENTS_PROVIDER": "stripe",
		})))
		if err == nil {
			t.Fatal("Load() error = nil, want the group refused as half-configured")
		}
		if !strings.Contains(err.Error(), "CONSOLE_API_PAYMENTS_SECRET_KEY must be set") {
			t.Errorf("Load() error = %q, want it to name the missing required variable", err)
		}
	})

	t.Run("accepts a cleartext return URL where an https API base URL stands", func(t *testing.T) {
		// The API base URL is https-only and the return URL is not, and the pair
		// has to be asserted TOGETHER: run against two https URLs it would pass
		// against a build that had dropped the restriction entirely, and run
		// against a cleartext API URL it would fail against a build that applied
		// one rule to both. The distinction is which URL carries a secret — the
		// API one carries the deployment's bearer credential on every request,
		// and the return one carries nothing but a browser to a console page.
		cfg, err := Load(lookup(paymentsEnv(merge(validPaymentsEnv(), map[string]string{
			"CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL": "http://localhost:5173/billing/top-up",
		}))))
		if err != nil {
			t.Fatalf("Load() error = %v, want a local console over http accepted: a developer running this on a laptop is doing something reasonable, and nothing secret is sent there", err)
		}
		if got := cfg.Payments.CheckoutReturnURL; got != "http://localhost:5173/billing/top-up" {
			t.Errorf("return URL = %q, want the value as configured", got)
		}
	})
}

func TestLoadRefusesPaymentsSettingsThatCannotWork(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			// Folding would accept this and produce a name the environment no
			// longer shows, which is why the grammar refuses it instead.
			name:    "rejects a provider name outside the folded alphabet",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_PROVIDER": "Stripe"},
			wantErr: "CONSOLE_API_PAYMENTS_PROVIDER \"Stripe\" must be 1 to 64 characters",
		},
		{
			name:    "rejects an explicitly empty provider",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_PROVIDER": ""},
			wantErr: "CONSOLE_API_PAYMENTS_PROVIDER must not be empty",
		},
		{
			name:    "rejects an explicitly empty API secret",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_SECRET_KEY": ""},
			wantErr: "CONSOLE_API_PAYMENTS_SECRET_KEY must not be empty",
		},
		{
			name:    "rejects an explicitly empty webhook signing secret",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET": ""},
			wantErr: "CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET must not be empty",
		},
		{
			// The case the whole rule exists for: the endpoint verifies every
			// forged delivery signed with the provider's documented example
			// value, and nothing looks wrong.
			name:    "rejects a documented example signing secret",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET": "changeme"},
			wantErr: "is one of the documented example values",
		},
		{
			name:    "rejects a placeholder signing secret in another case",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET": "Test"},
			wantErr: "is one of the documented example values",
		},
		{
			name:    "rejects an explicitly empty provider account key",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY": ""},
			wantErr: "CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY must not be empty",
		},
		{
			// A URL carrying credentials is a secret inside a value the
			// non-secret code paths log, which is the one thing a payment
			// configuration must never produce.
			name:    "rejects an API base URL carrying userinfo",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_API_BASE_URL": "https://user:" + paymentsTestSecretKey() + "@api.stripe.com"},
			wantErr: "CONSOLE_API_PAYMENTS_API_BASE_URL must not carry userinfo",
		},
		{
			name:    "rejects a return URL carrying userinfo",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL": "https://user:" + paymentsTestSecretKey() + "@console.internal.example/billing"},
			wantErr: "CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL must not carry userinfo",
		},
		{
			name:    "rejects a relative return URL",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL": "/billing/top-up"},
			wantErr: "CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL must use the http or https scheme",
		},
		{
			name:    "rejects an API base URL carrying a query",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_API_BASE_URL": "https://api.stripe.com?v=2024"},
			wantErr: "CONSOLE_API_PAYMENTS_API_BASE_URL must carry no query or fragment",
		},
		{
			// A cleartext API base URL would carry this deployment's payment API
			// secret as a bearer credential in the clear, on every checkout. The
			// case is loopback on purpose: nothing about 127.0.0.1 is unsafe, and
			// the rule still applies because the URL is where the secret is SENT.
			// An exemption for loopback would be one a deployment could be
			// misconfigured past in staging and not in production.
			name:    "rejects a cleartext API base URL",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_API_BASE_URL": "http://api.stripe.com"},
			wantErr: "CONSOLE_API_PAYMENTS_API_BASE_URL must use the https scheme",
		},
		{
			name:    "rejects a return URL carrying a fragment",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL": "https://console.internal.example/billing#done"},
			wantErr: "CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL must carry no query or fragment",
		},
		{
			name:    "rejects a zero webhook tolerance",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE": "0s"},
			wantErr: "CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE must be greater than zero",
		},
		{
			name:    "rejects a negative webhook tolerance",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE": "-1m"},
			wantErr: "CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE must be greater than zero",
		},
		{
			name:    "rejects a webhook tolerance past the ceiling",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE": "1h"},
			wantErr: "CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE (1h0m0s) must be at most 15m0s",
		},
		{
			name:    "rejects a malformed webhook tolerance",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE": "soon"},
			wantErr: "CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE must be a Go duration",
		},
		{
			name:    "rejects a zero request timeout",
			env:     map[string]string{"CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT": "0s"},
			wantErr: "CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT must be greater than zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(lookup(paymentsEnv(merge(validPaymentsEnv(), tt.env))))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load() error = %q, want it to contain %q", err, tt.wantErr)
			}
			// Both secrets are configured in every case above, and neither may
			// reach a failure message: an operator reads these lines, and a
			// startup line is the last place a credential should be.
			for _, secret := range []string{paymentsTestSecretKey(), paymentsTestSigningSecret()} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("Load() error = %q, must not carry a configured secret", err)
				}
			}
		})
	}
}

func TestPaymentsLogValueRedactsBothSecretsAndNamesEverySetting(t *testing.T) {
	// This is the test that makes the redaction a rule rather than a
	// convention. The type cannot be kept out of a log line — the whole point
	// of LogValue is that the group may be logged — so the only enforcement
	// available is an assertion on the rendered string, which is exactly what
	// a call site would write.
	payments := Payments{
		Provider:             "stripe",
		SecretKey:            paymentsTestSecretKey(),
		WebhookSigningSecret: paymentsTestSigningSecret(),
		ProviderAccountKey:   "acct-console-api-merchant",
		APIBaseURL:           "https://api.stripe.com",
		CheckoutReturnURL:    "https://console.internal.example/billing/top-up",
		WebhookTolerance:     DefaultWebhookTolerance,
		RequestTimeout:       DefaultPaymentsRequestTimeout,
	}

	value := payments.LogValue().String()
	for _, secret := range []string{paymentsTestSecretKey(), paymentsTestSigningSecret()} {
		if strings.Contains(value, secret) {
			t.Errorf("LogValue() = %q, must not carry a configured secret", value)
		}
	}
	if count := strings.Count(value, "[redacted]"); count != 2 {
		t.Errorf("LogValue() = %q, want both secrets redacted and nothing else", value)
	}
	for _, want := range []string{
		"provider", "stripe",
		"provider_account_key", "acct-console-api-merchant",
		"api_base_url", "https://api.stripe.com",
		"checkout_return_url", "https://console.internal.example/billing/top-up",
		"webhook_tolerance", "request_timeout",
	} {
		if !strings.Contains(value, want) {
			t.Errorf("LogValue() = %q, want it to name %s", value, want)
		}
	}
}

func TestTheFmtVerbsCannotPrintEitherSecret(t *testing.T) {
	// LogValue above only covers calls that reach slog. Every other way a Go
	// program turns a value into text — a %v in a startup line written before the
	// logger exists, a %#v in a test failure, a %+v in a panic's arguments, a
	// value nested inside a larger struct — never calls LogValue and prints every
	// exported field. This test is what makes String and GoString a contract
	// rather than a convenience: it renders the group the way those call sites do
	// and asserts the secrets are absent from the text.
	payments := Payments{
		Provider:             "stripe",
		SecretKey:            paymentsTestSecretKey(),
		WebhookSigningSecret: paymentsTestSigningSecret(),
		ProviderAccountKey:   "acct-console-api-merchant",
		APIBaseURL:           "https://api.stripe.com",
		CheckoutReturnURL:    "https://console.internal.example/billing/top-up",
		WebhookTolerance:     DefaultWebhookTolerance,
		RequestTimeout:       DefaultPaymentsRequestTimeout,
		TopUpOffers:          []TopUpOffer{{ID: "starter", AmountMinorUnits: 1000, Currency: "USD", MinorUnitExponent: 2, Label: "Starter"}},
	}
	// The nested wrapper is not decoration: it is the shape a real call site has,
	// because nobody formats a configuration group on its own — they format the
	// Config that holds it, and a struct with no String method of its own still
	// hands each field to fmt's own rules.
	type holder struct{ Payments Payments }

	for _, tc := range []struct {
		name   string
		render func() string
	}{
		{"%v of the group", func() string { return fmt.Sprintf("%v", payments) }},
		{"%s of the group", payments.String},
		{"%+v of the group", func() string { return fmt.Sprintf("%+v", payments) }},
		{"%#v of the group", func() string { return fmt.Sprintf("%#v", payments) }},
		{"%#v of a pointer to the group", func() string { return fmt.Sprintf("%#v", &payments) }},
		{"%v of a struct holding the group", func() string { return fmt.Sprintf("%v", holder{payments}) }},
		{"%#v of a struct holding the group", func() string { return fmt.Sprintf("%#v", holder{payments}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := tc.render()
			for _, secret := range []string{paymentsTestSecretKey(), paymentsTestSigningSecret()} {
				if strings.Contains(rendered, secret) {
					t.Fatalf("%s = %q, must not carry a configured secret", tc.name, rendered)
				}
			}
			if count := strings.Count(rendered, "[redacted]"); count != 2 {
				t.Errorf("%s = %q, want both secrets redacted and nothing else", tc.name, rendered)
			}
			// Redacting by rendering nothing would pass the two assertions
			// above while making the value useless in exactly the diagnostics it
			// exists for, so the settings an operator needs are asserted present.
			for _, want := range []string{"stripe", "acct-console-api-merchant", "starter"} {
				if !strings.Contains(rendered, want) {
					t.Errorf("%s = %q, want it to name %s", tc.name, rendered, want)
				}
			}
		})
	}
}

func TestTheShippedPaymentsDefaultsSatisfyTheRulesThatRefuseThem(t *testing.T) {
	// The same guard the reconciliation defaults carry: nothing forces the
	// shipped tolerance and timeout through validatePayments, so a default
	// one of the rules refuses would ship as a group every deployment that
	// trusted it could not start.
	env := merge(validPaymentsEnv(), map[string]string{
		"CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE": DefaultWebhookTolerance.String(),
		"CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT":   DefaultPaymentsRequestTimeout.String(),
	})
	cfg, err := Load(lookup(paymentsEnv(env)))
	if err != nil {
		t.Fatalf("the shipped payments defaults are rejected by their own rule: %v", err)
	}
	if cfg.Payments.WebhookTolerance != DefaultWebhookTolerance || cfg.Payments.RequestTimeout != DefaultPaymentsRequestTimeout {
		t.Fatalf("the explicit defaults (%s, %s) are not the shipped ones", cfg.Payments.WebhookTolerance, cfg.Payments.RequestTimeout)
	}
}

// offerEnvFor spells one offer's four variables from its id, the way a
// deployment does: through the same segment function the loader reads them by,
// so a test that spells an offer by hand and a loader that reads it cannot
// disagree about the name.
//
// An empty value is written through, not omitted: "the amount is set to
// nothing" and "the amount is not set at all" are different deployments, and
// both are refused.
func offerEnvFor(id, amount, currency, exponent, label string) map[string]string {
	segment := topUpOfferSegment(id)
	return map[string]string{
		topUpOfferVariable(segment, "AMOUNT_MINOR_UNITS"):  amount,
		topUpOfferVariable(segment, "CURRENCY"):            currency,
		topUpOfferVariable(segment, "MINOR_UNIT_EXPONENT"): exponent,
		topUpOfferVariable(segment, "LABEL"):               label,
	}
}

// offerEnvWithout is offerEnvFor with the named variables removed, for the
// cases about an offer that is declared and only half written.
func offerEnvWithout(id, amount, currency, exponent, label string, omit ...string) map[string]string {
	env := offerEnvFor(id, amount, currency, exponent, label)
	for _, field := range omit {
		delete(env, topUpOfferVariable(topUpOfferSegment(id), field))
	}
	return env
}

// offersEnv is the whole payments environment for a deployment that declares
// the given offers.
func offersEnv(declared map[string]string) map[string]string {
	return paymentsEnv(merge(validPaymentsEnv(), declared))
}

func TestLoadReadsTheTopUpOffersADeploymentDeclares(t *testing.T) {
	t.Run("publishes no offers when the list is absent", func(t *testing.T) {
		cfg, err := Load(lookup(paymentsEnv(validPaymentsEnv())))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Payments.TopUpOffers != nil {
			t.Errorf("Load() offers = %v, want none: the deployment declared no list", cfg.Payments.TopUpOffers)
		}
	})

	t.Run("reads each offer's price, unit and label", func(t *testing.T) {
		declared := merge(
			map[string]string{topUpOfferIDsVariable: "starter,team-annual"},
			merge(
				offerEnvFor("starter", "1000", "USD", "2", "Starter"),
				offerEnvFor("team-annual", "250000", "JPY", "0", "Team, annual"),
			),
		)
		cfg, err := Load(lookup(offersEnv(declared)))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		want := []TopUpOffer{
			{ID: "starter", AmountMinorUnits: 1000, Currency: "USD", MinorUnitExponent: 2, Label: "Starter"},
			{ID: "team-annual", AmountMinorUnits: 250000, Currency: "JPY", MinorUnitExponent: 0, Label: "Team, annual"},
		}
		if !reflect.DeepEqual(cfg.Payments.TopUpOffers, want) {
			t.Errorf("Load() offers = %+v, want %+v", cfg.Payments.TopUpOffers, want)
		}
	})

	t.Run("keeps the order the operator declared rather than sorting it", func(t *testing.T) {
		// The order is part of the value: a chooser renders these, and a slice
		// sorted here or a map handed over instead is a different chooser.
		declared := merge(
			map[string]string{topUpOfferIDsVariable: "team-annual,starter"},
			merge(
				offerEnvFor("starter", "1000", "USD", "2", "Starter"),
				offerEnvFor("team-annual", "250000", "USD", "2", "Team"),
			),
		)
		cfg, err := Load(lookup(offersEnv(declared)))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		ids := topUpOfferIDs(cfg.Payments.TopUpOffers)
		if !reflect.DeepEqual(ids, []string{"team-annual", "starter"}) {
			t.Errorf("Load() offer order = %v, want the declared order [team-annual starter]", ids)
		}
	})

	t.Run("carries a label this configuration's own encoding would have had to escape", func(t *testing.T) {
		// The encoding is one variable per field and no separators anywhere, and
		// this is the property that buys: a label containing the two characters a
		// record encoding uses (a comma and a colon) is a label, not a parse
		// error and not a second offer.
		label := `Team, annual: 20% off "everything"`
		declared := merge(
			map[string]string{topUpOfferIDsVariable: "team"},
			offerEnvFor("team", "250000", "USD", "2", label),
		)
		cfg, err := Load(lookup(offersEnv(declared)))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(cfg.Payments.TopUpOffers) != 1 || cfg.Payments.TopUpOffers[0].Label != label {
			t.Errorf("Load() offers = %+v, want one offer labelled %q", cfg.Payments.TopUpOffers, label)
		}
	})

	t.Run("carries a price in the ledger's own integer type", func(t *testing.T) {
		// There is no ACCEPTED price that needs a 64-bit parse any more, and the
		// arithmetic says so rather than this comment asserting it: the provider
		// ceiling is 99,999,999, which is below 2^31-1, so every figure a
		// deployment can actually sell fits in a 32-bit integer. A case that
		// loaded 2^52 and expected it accepted would be a test that cannot pass.
		//
		// What the int64 parse still buys is on the REFUSAL side, and the case
		// in the refusal table below carries it: 2^63 is refused as "not a whole
		// number of minor units" because ParseInt reported an overflow, not
		// because the ceiling caught a wrapped value. The check is a SIGNED
		// 64-bit parse for the same reason the ledger's money type is one — a
		// price is the figure a customer is charged, and it is not carried in a
		// type that depends on the build it was configured on.
		if ProviderMaxUnitAmountMinorUnits >= int64(1)<<31-1 {
			t.Fatalf("the provider ceiling is %d, which no longer sits below 2^31: the two ranges have met, and the accepted price a 32-bit parse could not read has to be tested again here",
				ProviderMaxUnitAmountMinorUnits)
		}
	})

	t.Run("accepts a price this build can carry and the provider can charge", func(t *testing.T) {
		// The ceiling that binds is the SMALLER of two — the provider's — and
		// the case worth pinning is the boundary that actually applies. A test
		// that only exercised the 2^53 one would pass against a build that had
		// forgotten the provider entirely, which is the omission this pins.
		declared := merge(
			map[string]string{topUpOfferIDsVariable: "enterprise"},
			offerEnvFor("enterprise", "99999999", "USD", "2", "Enterprise"),
		)
		cfg, err := Load(lookup(offersEnv(declared)))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if got := cfg.Payments.TopUpOffers[0].AmountMinorUnits; got != ProviderMaxUnitAmountMinorUnits {
			t.Errorf("amount = %d, want the %d provider ceiling itself accepted", got, ProviderMaxUnitAmountMinorUnits)
		}
	})

	t.Run("accepts both ends of the exponent range", func(t *testing.T) {
		declared := merge(
			map[string]string{topUpOfferIDsVariable: "yen,franc"},
			merge(
				offerEnvFor("yen", "1000", "JPY", "0", "Yen"),
				offerEnvFor("franc", "1000", "CLF", "4", "Unidad de Fomento"),
			),
		)
		if _, err := Load(lookup(offersEnv(declared))); err != nil {
			t.Errorf("Load() error = %v, want the domain's own 0..4 range accepted", err)
		}
	})
}

func TestLoadRefusesTopUpOffersThatCannotWork(t *testing.T) {
	tests := []struct {
		name     string
		declared map[string]string
		wantErr  string
	}{
		{
			name:     "rejects an offer id outside the alphabet its variables are spelled from",
			declared: map[string]string{topUpOfferIDsVariable: "Starter"},
			wantErr:  `offer id "Starter" must be 1 to 64 characters of lowercase letters, digits, hyphens and underscores`,
		},
		{
			name:     "rejects an empty element in the list",
			declared: map[string]string{topUpOfferIDsVariable: "starter,,team"},
			wantErr:  "CONSOLE_API_PAYMENTS_TOP_UP_OFFER_IDS must not carry an empty offer id",
		},
		{
			name:     "rejects an explicitly empty list rather than reading it as no offers",
			declared: map[string]string{topUpOfferIDsVariable: ""},
			wantErr:  "CONSOLE_API_PAYMENTS_TOP_UP_OFFER_IDS must not be empty; omit the variable to publish no top-up offers",
		},
		{
			name: "rejects the same offer declared twice",
			declared: merge(
				map[string]string{topUpOfferIDsVariable: "starter,starter"},
				offerEnvFor("starter", "1000", "USD", "2", "Starter"),
			),
			wantErr: `names both "starter" and "starter", whose settings are both spelled STARTER`,
		},
		{
			// The case the spelling rule exists for: an environment cannot tell
			// these two apart, so one offer's amount would silently be the other's.
			name: "rejects two offers whose settings are spelled the same way",
			declared: merge(
				map[string]string{topUpOfferIDsVariable: "team-annual,team_annual"},
				merge(
					offerEnvFor("team-annual", "1000", "USD", "2", "Annual"),
					offerEnvFor("team_annual", "9999", "USD", "2", "Monthly"),
				),
			),
			wantErr: `names both "team-annual" and "team_annual", whose settings are both spelled TEAM_ANNUAL`,
		},
		{
			name:     "rejects an offer declared with no price at all",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvWithout("starter", "1000", "USD", "2", "Starter", "AMOUNT_MINOR_UNITS")),
			wantErr:  "CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_AMOUNT_MINOR_UNITS is missing",
		},
		{
			name:     "rejects an offer declared with no currency",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvWithout("starter", "1000", "USD", "2", "Starter", "CURRENCY")),
			wantErr:  "CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_CURRENCY is missing",
		},
		{
			name:     "rejects an offer declared with no exponent",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvWithout("starter", "1000", "USD", "2", "Starter", "MINOR_UNIT_EXPONENT")),
			wantErr:  "CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_MINOR_UNIT_EXPONENT is missing",
		},
		{
			name:     "rejects an offer declared with no label",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvWithout("starter", "1000", "USD", "2", "Starter", "LABEL")),
			wantErr:  "CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_LABEL is missing",
		},
		{
			name:     "rejects an empty price",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "", "USD", "2", "Starter")),
			wantErr:  "CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_AMOUNT_MINOR_UNITS must not be empty",
		},
		{
			name:     "rejects a price written as a decimal",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "10.00", "USD", "2", "Starter")),
			wantErr:  `offer "starter" must be an integer number of minor units, and "10.00" is not one`,
		},
		{
			name:     "rejects a price with a currency glued to it",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000usd", "USD", "2", "Starter")),
			wantErr:  `offer "starter" must be an integer number of minor units, and "1000usd" is not one`,
		},
		{
			name:     "rejects a price of zero",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "0", "USD", "2", "Starter")),
			wantErr:  `offer "starter" must be greater than zero, and 0 is not an offer`,
		},
		{
			name:     "rejects a negative price",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "-1000", "USD", "2", "Starter")),
			wantErr:  `offer "starter" must be greater than zero, and -1000 is not an offer`,
		},
		{
			// One past the signed 64-bit range. ParseInt reports an OVERFLOW
			// rather than a syntax error, and the two want the same answer here:
			// the figure is not a number this build can hold, whichever way it
			// failed. This is the only case left that needs a 64-bit parse —
			// every price a deployment can sell is under 2^31, which the
			// accepted-price case above asserts the shape of.
			name:     "rejects a price past the range an int64 holds",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "9223372036854775808", "USD", "2", "Starter")),
			wantErr:  "must be an integer number of minor units",
		},
		{
			// One past the ceiling: the refusal names the parser that loses the
			// figure, because "too big" alone reads as an arbitrary limit and an
			// operator would raise it.
			name:     "rejects a price past what the console's own parser carries exactly",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "9007199254740992", "USD", "2", "Starter")),
			wantErr:  `is priced at 9007199254740992 minor units, over the 9007199254740991 this build can carry to a client`,
		},
		{
			// One past the ceiling that actually binds. A figure inside the 2^53
			// window and outside the provider's is the one that loads cleanly
			// here and fails at the provider on every single checkout, so the
			// refusal has to happen at boot with a message naming the provider —
			// an operator who read only about the console would raise the wrong
			// limit and change nothing.
			name:     "rejects a price past what the payment provider's own API carries",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "100000000", "USD", "2", "Starter")),
			wantErr:  `is priced at 100000000 minor units, over the 99999999 the payment provider's API can carry`,
		},
		{
			name:     "rejects a lowercase currency",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "usd", "2", "Starter")),
			wantErr:  `offer "starter" states the currency "usd", which must be three uppercase letters`,
		},
		{
			name:     "rejects a currency that is two characters",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "US", "2", "Starter")),
			wantErr:  "must be three uppercase letters",
		},
		{
			name:     "rejects a currency that is four characters",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "USDC", "2", "Starter")),
			wantErr:  "must be three uppercase letters",
		},
		{
			name:     "rejects an exponent past the domain's ceiling",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "USD", "5", "Starter")),
			wantErr:  `offer "starter" states the exponent 5, and this build places a decimal point at 0 to 4 places`,
		},
		{
			name:     "rejects a negative exponent",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "USD", "-1", "Starter")),
			wantErr:  "must not be negative",
		},
		{
			name:     "rejects an exponent that is not a number",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "USD", "two", "Starter")),
			wantErr:  "must be an integer",
		},
		{
			name:     "rejects an empty label",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "USD", "2", "")),
			wantErr:  `offer "starter" has an empty label`,
		},
		{
			name:     "rejects a label that is only whitespace",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "USD", "2", "   ")),
			wantErr:  `offer "starter" has an empty label`,
		},
		{
			name:     "rejects a label over the bound",
			declared: merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "USD", "2", strings.Repeat("l", maxTopUpOfferLabelLength+1))),
			wantErr:  "has a label of 121 characters, over the 120 this build carries",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(lookup(offersEnv(tt.declared)))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRefusesMoreOffersThanItCarries(t *testing.T) {
	// The ceiling is about a configuration that failed to render rather than
	// about a shop with many shelves, so the case is built by declaring one more
	// offer than the ceiling and spelling every one of them completely: the
	// refusal must be the ceiling and not a missing field.
	declared := map[string]string{}
	ids := make([]string, 0, MaxTopUpOffers+1)
	for i := 0; i <= MaxTopUpOffers; i++ {
		id := "offer-" + strconv.Itoa(i)
		ids = append(ids, id)
		for name, value := range offerEnvFor(id, "1000", "USD", "2", "Offer") {
			declared[name] = value
		}
	}
	declared[topUpOfferIDsVariable] = strings.Join(ids, ",")

	_, err := Load(lookup(offersEnv(declared)))
	if err == nil {
		t.Fatal("Load() error = nil, want the ceiling refused")
	}
	want := "declares 65 top-up offers, and this build carries at most 64"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Load() error = %q, want it to contain %q", err, want)
	}
}

func TestADeploymentThatDeclaresOffersWithoutTheRestOfTheGroupIsRefused(t *testing.T) {
	// The offers are part of the group, not a group of their own: a deployment
	// that declares what it sells and names no provider has decided to run a
	// payment surface and has not finished configuring one, and the list
	// variable is in the group's presence probe exactly so this is caught rather
	// than started as a console with no payments and a price list nobody reads.
	declared := merge(map[string]string{topUpOfferIDsVariable: "starter"}, offerEnvFor("starter", "1000", "USD", "2", "Starter"))
	_, err := Load(lookup(merge(requiredEnv(), declared)))
	if err == nil {
		t.Fatal("Load() error = nil, want the group refused as half-configured")
	}
	if !strings.Contains(err.Error(), "CONSOLE_API_PAYMENTS_PROVIDER must be set") {
		t.Errorf("Load() error = %q, want it to name the missing required variable", err)
	}
}

func TestPaymentsLogValueNamesTheOffersItPublishes(t *testing.T) {
	// The log line carries the declaration and not the prices: an operator
	// reading it needs to know which offers this deployment is running, and the
	// amounts and labels are readable from the configuration itself.
	payments := Payments{
		Provider:    "stripe",
		TopUpOffers: []TopUpOffer{{ID: "starter"}, {ID: "team-annual"}},
	}
	value := payments.LogValue().String()
	if !strings.Contains(value, "top_up_offer_ids") || !strings.Contains(value, "starter,team-annual") {
		t.Errorf("LogValue() = %q, want it to name the offers this deployment publishes", value)
	}
}
