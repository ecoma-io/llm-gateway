package catalog

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewAliasAssignsPositionsAndStaysInTheBirthState(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	id, err := NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID returned error: %v", err)
	}
	alias, err := NewAlias(id, " gpt-5 ", 4096, 1024, []Candidate{
		{BackendID: "b1", ProviderModel: "gpt-5"},
		{BackendID: "b2", ProviderModel: "gpt-5-mini", ParameterOverrides: json.RawMessage(`{"temperature":0.2}`)},
	}, now)
	if err != nil {
		t.Fatalf("NewAlias returned error: %v", err)
	}
	if alias.Name != "gpt-5" {
		t.Errorf("Name = %q, want the trimmed value", alias.Name)
	}
	if alias.State != AliasActive {
		t.Errorf("State = %q, want the birth state active", alias.State)
	}
	if alias.RetiredAt != nil {
		t.Errorf("RetiredAt = %v, want nil in the birth state", alias.RetiredAt)
	}
	for i, want := range []int{1, 2} {
		if alias.Candidates[i].Position != want {
			t.Errorf("candidate %d Position = %d, want %d — slice order is fallback order", i, alias.Candidates[i].Position, want)
		}
		if alias.Candidates[i].ID == "" {
			t.Errorf("candidate %d carries no minted id", i)
		}
	}
	if string(alias.Candidates[1].ParameterOverrides) != `{"temperature":0.2}` {
		t.Errorf("overrides = %s, want the object passed through", alias.Candidates[1].ParameterOverrides)
	}
	if alias.Candidates[0].ParameterOverrides != nil {
		t.Errorf("absent overrides = %s, want nil", alias.Candidates[0].ParameterOverrides)
	}
}

func TestNewAliasRefusesNamesOutsideTheGrammar(t *testing.T) {
	for name, bad := range map[string]string{
		"blank":          "",
		"whitespace":     "   ",
		"wildcard":       "*",
		"leading dot":    ".gpt-5",
		"leading slash":  "/gpt-5",
		"unicode":        "mô-hình",
		"interior space": "gpt 5",
		"too long":       strings.Repeat("a", 129),
		"one rune over":  strings.Repeat("x", 9) + strings.Repeat("y", 120),
		"control char":   "gpt-\x005",
	} {
		id, err := NewAliasID()
		if err != nil {
			t.Fatalf("NewAliasID returned error: %v", err)
		}
		_, err = NewAlias(id, bad, 4096, 1024, []Candidate{{BackendID: "b1", ProviderModel: "m"}}, time.Now())
		if err == nil {
			t.Fatalf("%s: NewAlias accepted %q", name, bad)
		}
		if !errors.Is(err, ErrInvalidAliasName) {
			t.Errorf("%s: error = %v, want ErrInvalidAliasName", name, err)
		}
	}
}

func TestNewAliasAcceptsTheNamesClientsActuallySend(t *testing.T) {
	for name, good := range map[string]string{
		"plain":             "gpt-5",
		"kebab":             "claude-sonnet-5",
		"dotted":            "gpt-5.1",
		"slashed":           "meta.llama3/70b",
		"underscored":       "o3_mini",
		"exactly 128 runes": strings.Repeat("a", 128),
	} {
		id, err := NewAliasID()
		if err != nil {
			t.Fatalf("NewAliasID returned error: %v", err)
		}
		if _, err := NewAlias(id, good, 4096, 1024, []Candidate{{BackendID: "b1", ProviderModel: "m"}}, time.Now()); err != nil {
			t.Errorf("%s (%q): NewAlias returned error: %v", name, good, err)
		}
	}
}

func TestNewAliasRefusesNonPositiveBounds(t *testing.T) {
	for name, bounds := range map[string][2]int64{
		"zero output limit":        {0, 100},
		"negative output limit":    {-1, 100},
		"zero reservation cap":     {100, 0},
		"negative reservation cap": {100, -5},
		"both zero":                {0, 0},
	} {
		id, err := NewAliasID()
		if err != nil {
			t.Fatalf("NewAliasID returned error: %v", err)
		}
		_, err = NewAlias(id, "gpt-5", bounds[0], bounds[1], []Candidate{{BackendID: "b1", ProviderModel: "m"}}, time.Now())
		if !errors.Is(err, ErrInvalidBounds) {
			t.Errorf("%s: error = %v, want ErrInvalidBounds", name, err)
		}
	}
}

func TestCandidateListsAreRefusedBeforeAnyStatementRuns(t *testing.T) {
	for name, candidates := range map[string][]Candidate{
		"empty":                   {},
		"nil":                     nil,
		"blank backend":           {{BackendID: "", ProviderModel: "m"}},
		"blank provider model":    {{BackendID: "b1", ProviderModel: "  "}},
		"duplicate target":        {{BackendID: "b1", ProviderModel: "m"}, {BackendID: "b1", ProviderModel: "m"}},
		"provider model too long": {{BackendID: "b1", ProviderModel: strings.Repeat("m", 257)}},
		"overrides are an array":  {{BackendID: "b1", ProviderModel: "m", ParameterOverrides: json.RawMessage(`[1,2]`)}},
		"overrides are not json":  {{BackendID: "b1", ProviderModel: "m", ParameterOverrides: json.RawMessage(`{oops`)}},
		"overrides are a scalar":  {{BackendID: "b1", ProviderModel: "m", ParameterOverrides: json.RawMessage(`42`)}},
	} {
		id, err := NewAliasID()
		if err != nil {
			t.Fatalf("NewAliasID returned error: %v", err)
		}
		_, err = NewAlias(id, "gpt-5", 4096, 1024, candidates, time.Now())
		if err == nil {
			t.Fatalf("%s: NewAlias accepted the list", name)
		}
		if !errors.Is(err, ErrInvalidCandidates) && !errors.Is(err, ErrInvalidParameterOverrides) {
			t.Errorf("%s: error = %v, want a candidate-shaped sentinel", name, err)
		}
	}
}

func TestSameBackendServingDifferentModelsIsNotADuplicate(t *testing.T) {
	id, err := NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID returned error: %v", err)
	}
	alias, err := NewAlias(id, "gpt-5", 4096, 1024, []Candidate{
		{BackendID: "b1", ProviderModel: "gpt-5"},
		{BackendID: "b1", ProviderModel: "gpt-5-mini"},
	}, time.Now())
	if err != nil {
		t.Fatalf("NewAlias returned error: %v — one backend may serve two models in one fallback order", err)
	}
	if len(alias.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(alias.Candidates))
	}
}

func TestRetirementFreezesTheAggregate(t *testing.T) {
	id, err := NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID returned error: %v", err)
	}
	alias, err := NewAlias(id, "gpt-5", 4096, 1024, []Candidate{{BackendID: "b1", ProviderModel: "gpt-5"}}, time.Now())
	if err != nil {
		t.Fatalf("NewAlias returned error: %v", err)
	}
	retired := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := alias.Retire(retired); err != nil {
		t.Fatalf("Retire returned error: %v", err)
	}
	if alias.State != AliasRetired || alias.RetiredAt == nil || !alias.RetiredAt.Equal(retired) {
		t.Fatalf("retirement did not stamp the aggregate: state %q, retired_at %v", alias.State, alias.RetiredAt)
	}
	// Retirement is one-way and idempotent: a second call is a no-op, and
	// the stamp does not move.
	if err := alias.Retire(retired.Add(time.Hour)); err != nil {
		t.Errorf("second Retire returned error: %v — terminal states absorb repetition", err)
	}
	if !alias.RetiredAt.Equal(retired) {
		t.Errorf("RetiredAt = %v, want the first retirement's instant", alias.RetiredAt)
	}
	// The freeze: no configuration move lands on a retired alias, whatever
	// the caller wanted to write.
	if err := alias.SetCandidates([]Candidate{{BackendID: "b2", ProviderModel: "m"}}, time.Now()); !errors.Is(err, ErrAliasRetired) {
		t.Errorf("SetCandidates on retired error = %v, want ErrAliasRetired", err)
	}
	if err := alias.UpdateBounds(8192, 2048, time.Now()); !errors.Is(err, ErrAliasRetired) {
		t.Errorf("UpdateBounds on retired error = %v, want ErrAliasRetired", err)
	}
	if len(alias.Candidates) != 1 || alias.Candidates[0].BackendID != "b1" {
		t.Errorf("candidates moved under the freeze: %+v", alias.Candidates)
	}
	if alias.MaxOutputTokens != 4096 {
		t.Errorf("bounds moved under the freeze: %d", alias.MaxOutputTokens)
	}
}

func TestSetCandidatesReplacesTheWholeList(t *testing.T) {
	id, err := NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID returned error: %v", err)
	}
	alias, err := NewAlias(id, "gpt-5", 4096, 1024, []Candidate{{BackendID: "b1", ProviderModel: "gpt-5"}}, time.Now())
	if err != nil {
		t.Fatalf("NewAlias returned error: %v", err)
	}
	now := time.Date(2026, 9, 25, 13, 0, 0, 0, time.UTC)
	if err := alias.SetCandidates([]Candidate{
		{BackendID: "b2", ProviderModel: "gpt-5"},
		{BackendID: "b1", ProviderModel: "gpt-5"},
	}, now); err != nil {
		t.Fatalf("SetCandidates returned error: %v", err)
	}
	if len(alias.Candidates) != 2 {
		t.Fatalf("candidates = %d, want the replacement's 2", len(alias.Candidates))
	}
	// New list, positions renumbered from one, fresh identities: a replaced
	// candidate is a new row, not a patched one.
	if alias.Candidates[0].BackendID != "b2" || alias.Candidates[0].Position != 1 {
		t.Errorf("first candidate = %+v, want b2 at position 1", alias.Candidates[0])
	}
	if alias.Candidates[0].ID == alias.Candidates[1].ID {
		t.Errorf("replacement minted colliding candidate ids")
	}
	// And the same shape rules as birth apply to the replacement.
	if err := alias.SetCandidates(nil, now); !errors.Is(err, ErrInvalidCandidates) {
		t.Errorf("SetCandidates(nil) error = %v, want ErrInvalidCandidates", err)
	}
}

func TestUpdateBoundsRepointsAdmissionsArithmetic(t *testing.T) {
	id, err := NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID returned error: %v", err)
	}
	alias, err := NewAlias(id, "gpt-5", 4096, 1024, []Candidate{{BackendID: "b1", ProviderModel: "gpt-5"}}, time.Now())
	if err != nil {
		t.Fatalf("NewAlias returned error: %v", err)
	}
	now := time.Date(2026, 9, 25, 13, 0, 0, 0, time.UTC)
	if err := alias.UpdateBounds(8192, 2048, now); err != nil {
		t.Fatalf("UpdateBounds returned error: %v", err)
	}
	if alias.MaxOutputTokens != 8192 || alias.ReservationCap != 2048 {
		t.Fatalf("bounds = %d/%d, want 8192/2048", alias.MaxOutputTokens, alias.ReservationCap)
	}
	if err := alias.UpdateBounds(0, 2048, now); !errors.Is(err, ErrInvalidBounds) {
		t.Errorf("UpdateBounds with a zero bound error = %v, want ErrInvalidBounds", err)
	}
	if alias.MaxOutputTokens != 8192 {
		t.Errorf("a refused move still moved the value: %d", alias.MaxOutputTokens)
	}
}
