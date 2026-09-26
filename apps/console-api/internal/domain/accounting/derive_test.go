package accounting

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

// The Control-side re-derivation must agree with the Data Plane's hold
// formula figure for figure: the two are the same arithmetic on the two
// shores of one contract, and the golden literals below are the Data
// Plane's own (apps/dataplane/internal/domain/accounting hold_test.go).
// Either side changing a value here or there without the other is the
// defect this file exists to catch.
func TestSettledAmountAppliesOneCeilingOverTheSummedProduct(t *testing.T) {
	// bigCeiling is the oracle: one ceiling over the summed raw product,
	// computed in arbitrary precision by math/big rather than by the code
	// under test.
	bigCeiling := func(t *testing.T, inTokens, outTokens, inPrice, outPrice int64) int64 {
		t.Helper()
		sum := new(big.Int).Mul(big.NewInt(inTokens), big.NewInt(inPrice))
		sum.Add(sum, new(big.Int).Mul(big.NewInt(outTokens), big.NewInt(outPrice)))
		denom := big.NewInt(1_000_000)
		q, r := new(big.Int).QuoRem(sum, denom, new(big.Int))
		if r.Sign() != 0 {
			q.Add(q, big.NewInt(1))
		}
		if !q.IsInt64() {
			t.Fatalf("oracle quotient %s does not fit int64", q)
		}
		return q.Int64()
	}
	cases := []struct {
		name      string
		inTokens  int64
		outTokens int64
		inPrice   int64
		outPrice  int64
		want      int64
	}{
		{name: "exact multiples on both legs", inTokens: 1_000_000, outTokens: 2_000_000, inPrice: 1500, outPrice: 6000, want: 13500},
		{name: "a fractional ceiling over the sum", inTokens: 999_999, outTokens: 0, inPrice: 1, outPrice: 0, want: 1},
		{name: "two sub-unit arms that stay below one minor unit", inTokens: 499_999, outTokens: 500_000, inPrice: 1, outPrice: 1, want: 1},
		{name: "two sub-unit arms that ceil past one", inTokens: 499_999, outTokens: 500_002, inPrice: 1, outPrice: 1, want: 2},
		{name: "a zero-priced input leg drops out of the sum", inTokens: 5_000_000, outTokens: 1, inPrice: 0, outPrice: 2_000_000, want: 2},
		{name: "a free model settles for nothing", inTokens: 10_000, outTokens: 10_000, inPrice: 0, outPrice: 0, want: 0},
		{name: "no tokens settle for nothing", inTokens: 0, outTokens: 0, inPrice: 1500, outPrice: 6000, want: 0},
		{name: "a large settle that stays inside the widths", inTokens: 1_000_000_000, outTokens: 1_000_000_000, inPrice: 4_000_000_000, outPrice: 5_000_000_000, want: 9_000_000_000_000},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SettledAmount(tt.inTokens, tt.outTokens, tt.inPrice, tt.outPrice)
			if err != nil {
				t.Fatalf("SettledAmount() error = %v, want nil", err)
			}
			if got.Int64() != tt.want {
				t.Errorf("SettledAmount() = %d, want %d per the arbitrary-precision derivation", got.Int64(), tt.want)
			}
			if want := bigCeiling(t, tt.inTokens, tt.outTokens, tt.inPrice, tt.outPrice); got.Int64() != want {
				t.Errorf("SettledAmount() = %d, want %d from math/big", got.Int64(), want)
			}
		})
	}
}

func TestSettledAmountRefusesNegativeInputs(t *testing.T) {
	cases := []struct {
		name                string
		inTokens, outTokens int64
		inPrice, outPrice   int64
	}{
		{"a negative input count", -1, 0, 100, 100},
		{"a negative output count", 0, -1, 100, 100},
		{"a negative input price", 1, 1, -100, 100},
		{"a negative output price", 1, 1, 100, -100},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			amount, err := SettledAmount(tt.inTokens, tt.outTokens, tt.inPrice, tt.outPrice)
			if !errors.Is(err, ErrDerivationInputs) {
				t.Fatalf("SettledAmount() error = %v, want ErrDerivationInputs", err)
			}
			if amount != 0 {
				t.Errorf("SettledAmount() = %d, want 0 beside the error", amount)
			}
		})
	}
}

func TestSettledAmountReportsOverflowInsteadOfWrapping(t *testing.T) {
	// int64 inputs cannot push the 128-bit SUM past 128 bits — the widest
	// pair of arms still fits — but the quotient and its ceiling can both
	// pass 64 bits, and those are the refusals a caller can actually meet.
	// The fact that carries figures like these was never reserved by any
	// reservation cap, which is the sentence the wrapped error must say.
	amount, err := SettledAmount(math.MaxInt64, math.MaxInt64, 10_000_000, 10_000_000)
	if !errors.Is(err, ErrDerivationOverflow) {
		t.Fatalf("SettledAmount() error = %v, want ErrDerivationOverflow", err)
	}
	if amount != 0 {
		t.Errorf("SettledAmount() = %d, want 0 beside the error", amount)
	}
}
