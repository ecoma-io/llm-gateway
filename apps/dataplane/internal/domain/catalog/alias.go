package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// AliasState is an alias's position on its lifecycle: active or retired, with
// retirement one-way. A retired alias is history the ledger still resolves —
// usage facts and entitlement scopes name it — so its row never leaves, its
// name is never reissued, and its configuration never changes again.
type AliasState string

const (
	// AliasActive: the runtime resolves this alias; admission reads its
	// bounds; its candidate list may be edited.
	AliasActive AliasState = "active"
	// AliasRetired: admission answers unknown_alias; the aggregate is
	// frozen exactly as it was when retired.
	AliasRetired AliasState = "retired"
)

// maxAliasNameLen bounds the client-visible name. The name is request
// surface vocabulary — clients send it on every call — so the bound is a
// rune count, not a byte count, and the grammar is deliberately narrow.
const maxAliasNameLen = 128

// maxProviderModelLen bounds the provider's own model identifier. It is the
// provider's vocabulary passed through, so the bound is generous and the
// grammar is none — the provider rejects what it does not know.
const maxProviderModelLen = 256

// aliasNamePattern is the alias-name grammar: one alphanumeric head, then
// alphanumerics, dots, underscores, slashes and dashes — the shape real
// model names ("gpt-5", "claude-sonnet-5", "meta.llama3/70b") already have,
// minus everything that would make a log line or a metric label interesting.
// Group names share it, with `*` reserved (see group.go).
var aliasNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// Candidate is one entry in an alias's fallback order: which backend to call
// and what to call its provider's model. Positions are the alias's business,
// not the candidate's — a Candidate is constructed without one, and the
// alias assigns 1..n in slice order — because a position only means anything
// relative to the list it sits in.
//
// ParameterOverrides are the one place the catalog carries provider-shaped
// JSON. They are validated to be a JSON object and nothing more — opaque to
// the gateway, never queried, passed through at execution (the sanctioned
// second JSONB category in persistence.md) — because their meaning is the
// provider's, and a gateway that started interpreting them would be
// reimplementing a provider API it does not serve.
type Candidate struct {
	ID                 CandidateID
	BackendID          BackendID
	ProviderModel      string
	Position           int
	ParameterOverrides json.RawMessage
}

// ModelAlias is the catalog's client-facing aggregate: the logical name a
// request names, the bounds admission enforces before a candidate is chosen,
// and the ordered candidate list that resolves it (ADR 0002). Alias and
// candidates are ONE aggregate (ADR 0001, rule 3): the list is written and
// replaced atomically with the alias's own row, and every method below that
// touches it refuses on a retired alias, because retirement freezes the
// aggregate exactly as its historical requests used it.
type ModelAlias struct {
	ID              AliasID
	Name            string
	State           AliasState
	MaxOutputTokens int64
	ReservationCap  int64
	Candidates      []Candidate
	CreatedAt       time.Time
	UpdatedAt       time.Time
	RetiredAt       *time.Time
}

// NewAlias returns an alias in its birth state, active, carrying the
// candidate list exactly as given — slice order becomes fallback order,
// positions 1..n assigned here. The list must be non-empty, every candidate
// must name a backend and a provider model, and no target may repeat: an
// alias whose fallback order contains the same (backend, model) twice would
// retry failure into the same failure.
func NewAlias(id AliasID, name string, maxOutputTokens, reservationCap int64, candidates []Candidate, now time.Time) (*ModelAlias, error) {
	name = trimSpace(name)
	if err := validateAliasName(name); err != nil {
		return nil, err
	}
	if err := validateBounds(maxOutputTokens, reservationCap); err != nil {
		return nil, err
	}
	if id == "" {
		// A blank id is a programming error upstream (NewAliasID cannot
		// produce one), not a domain rule violation — no sentinel.
		return nil, fmt.Errorf("catalog: new alias: blank id")
	}
	assigned, err := assignCandidates(candidates)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	return &ModelAlias{
		ID:              id,
		Name:            name,
		State:           AliasActive,
		MaxOutputTokens: maxOutputTokens,
		ReservationCap:  reservationCap,
		Candidates:      assigned,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// SetCandidates replaces the aggregate's whole candidate list. There is no
// per-candidate edit on purpose: the list IS the fallback policy, position
// numbers only mean something together, and a partial edit would leave the
// aggregate explaining a policy no one wrote. Slice order becomes positions
// 1..n, exactly as at birth. Refused on a retired alias — retirement froze
// this aggregate.
func (a *ModelAlias) SetCandidates(candidates []Candidate, now time.Time) error {
	if a.State == AliasRetired {
		return fmt.Errorf("catalog: set candidates on alias %s: %w", a.ID, ErrAliasRetired)
	}
	assigned, err := assignCandidates(candidates)
	if err != nil {
		return err
	}
	a.Candidates = assigned
	a.UpdatedAt = now.UTC()
	return nil
}

// UpdateBounds re-points the output limit and the reservation cap admission
// reads. Refused on a retired alias, for the same freeze.
func (a *ModelAlias) UpdateBounds(maxOutputTokens, reservationCap int64, now time.Time) error {
	if a.State == AliasRetired {
		return fmt.Errorf("catalog: update bounds on alias %s: %w", a.ID, ErrAliasRetired)
	}
	if err := validateBounds(maxOutputTokens, reservationCap); err != nil {
		return err
	}
	a.MaxOutputTokens = maxOutputTokens
	a.ReservationCap = reservationCap
	a.UpdatedAt = now.UTC()
	return nil
}

// Retire takes the alias out of resolution, one-way. Retiring an
// already-retired alias is a no-op, not an error: terminal states absorb
// repetition, which keeps retry paths honest without an existence probe.
// Nothing about the row is deleted — the name is never reissued, and the
// candidates stay exactly as they were, because that is what "history
// resolves to the same identity it named" requires.
func (a *ModelAlias) Retire(now time.Time) error {
	switch a.State {
	case AliasRetired:
		return nil
	case AliasActive:
		retired := now.UTC()
		a.State = AliasRetired
		a.RetiredAt = &retired
		a.UpdatedAt = retired
		return nil
	default:
		return fmt.Errorf("catalog: retire alias %s: %w: unknown state %q", a.ID, ErrInvalidTransition, a.State)
	}
}

// assignCandidates validates a candidate list and returns it with positions
// assigned 1..n in slice order and identities minted. It is the one place
// the list's shape rules live, which is what makes NewAlias and
// SetCandidates the same rule and not two.
func assignCandidates(candidates []Candidate) ([]Candidate, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("catalog: assign candidates: %w: list is empty", ErrInvalidCandidates)
	}
	assigned := make([]Candidate, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for i, c := range candidates {
		if c.BackendID == "" {
			return nil, fmt.Errorf("catalog: assign candidates: %w: candidate %d names no backend", ErrInvalidCandidates, i)
		}
		model := trimSpace(c.ProviderModel)
		if model == "" {
			return nil, fmt.Errorf("catalog: assign candidates: %w: candidate %d names no provider model", ErrInvalidCandidates, i)
		}
		if utf8.RuneCountInString(model) > maxProviderModelLen {
			return nil, fmt.Errorf("catalog: assign candidates: %w: candidate %d provider model exceeds %d runes", ErrInvalidCandidates, i, maxProviderModelLen)
		}
		key := string(c.BackendID) + "\x00" + model
		if seen[key] {
			return nil, fmt.Errorf("catalog: assign candidates: %w: backend %s serving %s listed twice", ErrInvalidCandidates, c.BackendID, model)
		}
		seen[key] = true
		overrides, err := validateOverrides(c.ParameterOverrides)
		if err != nil {
			return nil, err
		}
		id := c.ID
		if id == "" {
			minted, err := newCandidateID()
			if err != nil {
				return nil, fmt.Errorf("catalog: assign candidates: %w", err)
			}
			id = minted
		}
		assigned[i] = Candidate{
			ID:                 id,
			BackendID:          c.BackendID,
			ProviderModel:      model,
			Position:           i + 1,
			ParameterOverrides: overrides,
		}
	}
	return assigned, nil
}

// validateOverrides passes a JSON object through and refuses anything else.
// nil is absence, not an error.
func validateOverrides(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, fmt.Errorf("catalog: assign candidates: %w", ErrInvalidParameterOverrides)
	}
	return json.RawMessage(trimmed), nil
}

// validateAliasName enforces the name grammar. The name is matched exactly
// as registered — no case folding — because clients send it verbatim and a
// gateway that quietly lowercased it would own a normalisation rule its
// operators never wrote.
func validateAliasName(name string) error {
	if !aliasNamePattern.MatchString(name) {
		return fmt.Errorf("catalog: new alias: %w: %q is outside the alias grammar", ErrInvalidAliasName, name)
	}
	if n := utf8.RuneCountInString(name); n > maxAliasNameLen {
		return fmt.Errorf("catalog: new alias: %w: %d runes exceeds %d", ErrInvalidAliasName, n, maxAliasNameLen)
	}
	return nil
}

// validateBounds enforces the one rule both bounds share: they bound real
// admission arithmetic, so they must be positive.
func validateBounds(maxOutputTokens, reservationCap int64) error {
	if maxOutputTokens <= 0 || reservationCap <= 0 {
		return fmt.Errorf("catalog: new alias: %w: max_output_tokens %d, reservation_cap %d", ErrInvalidBounds, maxOutputTokens, reservationCap)
	}
	return nil
}
