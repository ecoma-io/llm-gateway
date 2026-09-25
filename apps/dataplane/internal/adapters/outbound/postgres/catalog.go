package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The catalog repositories: translation between the persistence port's
// catalog vocabulary and the `dataplane` database's catalog tables. Every
// query resolves its handle through the store — the caller's unit of work
// when the context carries one, the pool otherwise — which is not a courtesy
// here but the schema's own requirement: an alias and its candidates are one
// aggregate written in one transaction (ADR 0001, rule 3), and only a
// Querier resolved from the writing context lands on that transaction.
//
// The boundary this file deliberately does not cross: none of these tables
// is the Control Plane's business. Entitlements over there reference a group
// version by id alone (ADR 0006 §7); no statement here names, joins, or can
// even parse against a control-lane table — the `control` database is a
// separate connection target, and this file never learns where.
//
// pgUniqueViolation, sqlStater and sqlStateOf are this package's copy of the
// SQLState seam the console-api adapter carries: the two modules cannot
// share unexported helpers, and each adapter classifies its own driver
// errors rather than importing a driver type to do it.

// pgUniqueViolation is PostgreSQL's SQLSTATE for a uniqueness constraint
// violation. Detected through the SQLState seam below rather than a driver
// type, so the mapping survives whichever database/sql driver the process
// boundary wires in — both drivers this module could reasonably adopt report
// SQLSTATE through the same interface{} shape.
const pgUniqueViolation = "23505"

// sqlStater is the seam for driver errors that carry a SQLSTATE: any error
// value whose chain holds one is a database-reported condition the adapter
// may classify. It is deliberately unexported and structural — a capability
// the driver either has or has not — rather than an import of one driver's
// error type.
type sqlStater interface {
	SQLState() string
}

// sqlStateOf returns the SQLSTATE the error chain carries, if any.
func sqlStateOf(err error) (string, bool) {
	var s sqlStater
	if errors.As(err, &s) {
		return s.SQLState(), true
	}
	return "", false
}

// NewBackends returns the persistence port's Backends repository backed by
// store. It panics on a nil store for the same reason the store panics on a
// nil pool: the failure a nil dependency produces later is strictly worse
// than a loud one here.
func NewBackends(store persistence.Store) persistence.Backends {
	if store == nil {
		panic("postgres: NewBackends requires a non-nil persistence.Store")
	}
	return &backendRepo{store: store}
}

// NewModelAliases returns the persistence port's ModelAliases repository
// backed by store.
func NewModelAliases(store persistence.Store) persistence.ModelAliases {
	if store == nil {
		panic("postgres: NewModelAliases requires a non-nil persistence.Store")
	}
	return &aliasRepo{store: store}
}

// NewAliasGroupVersions returns the persistence port's AliasGroupVersions
// repository backed by store.
func NewAliasGroupVersions(store persistence.Store) persistence.AliasGroupVersions {
	if store == nil {
		panic("postgres: NewAliasGroupVersions requires a non-nil persistence.Store")
	}
	return &groupVersionRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.Backends           = (*backendRepo)(nil)
	_ persistence.ModelAliases       = (*aliasRepo)(nil)
	_ persistence.AliasGroupVersions = (*groupVersionRepo)(nil)
)

// ---------------------------------------------------------------------------
// backends
// ---------------------------------------------------------------------------

type backendRepo struct {
	store persistence.Store
}

const insertBackend = `
INSERT INTO backends (id, adapter_type, endpoint, credentials_ref, egress_policy_ref, state, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

const selectBackend = `
SELECT id, adapter_type, endpoint, credentials_ref, egress_policy_ref, state, created_at, updated_at
FROM backends
WHERE id = $1`

// The compare-and-swap is the whole concurrency story: the move happens only
// while the row still shows the state the caller read, so two racing
// transitions converge instead of one silently overwriting the other.
const transitionBackend = `
UPDATE backends
SET state = $3, updated_at = $4
WHERE id = $1 AND state = $2`

const updateBackendTarget = `
UPDATE backends
SET endpoint = $2, credentials_ref = $3, egress_policy_ref = $4, updated_at = $5
WHERE id = $1`

func (r *backendRepo) Create(ctx context.Context, backend *catalog.Backend) error {
	_, err := r.store.Querier(ctx).ExecContext(ctx, insertBackend,
		string(backend.ID), backend.AdapterType, backend.Endpoint,
		nullRef(backend.CredentialsRef), nullRef(backend.EgressPolicyRef),
		string(backend.State), backend.CreatedAt, backend.UpdatedAt)
	if err != nil {
		return fmt.Errorf("postgres: create backend %s: %w", backend.ID, err)
	}
	return nil
}

func (r *backendRepo) ByID(ctx context.Context, id catalog.BackendID) (*catalog.Backend, error) {
	var b catalog.Backend
	var state string
	var credentialsRef, egressPolicyRef sql.NullString
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectBackend, string(id)).
		Scan(&b.ID, &b.AdapterType, &b.Endpoint, &credentialsRef, &egressPolicyRef, &state, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("postgres: backend %s: %w", id, persistence.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: backend %s: %w", id, err)
	}
	b.CredentialsRef = credentialsRef.String
	b.EgressPolicyRef = egressPolicyRef.String
	b.State = catalog.BackendState(state)
	return &b, nil
}

func (r *backendRepo) TransitionState(ctx context.Context, id catalog.BackendID, from, to catalog.BackendState, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, transitionBackend,
		string(id), string(from), string(to), updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: transition backend %s %s->%s: %w", id, from, to, err)
	}
	return rowsApplied(res, "transition backend", id)
}

func (r *backendRepo) UpdateTarget(ctx context.Context, id catalog.BackendID, endpoint, credentialsRef, egressPolicyRef string, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, updateBackendTarget,
		string(id), endpoint, nullRef(credentialsRef), nullRef(egressPolicyRef), updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: update backend %s target: %w", id, err)
	}
	return rowsApplied(res, "update backend", id)
}

// ---------------------------------------------------------------------------
// model_aliases — the aggregate read and write is alias row plus candidates.
// ---------------------------------------------------------------------------

type aliasRepo struct {
	store persistence.Store
}

const insertAlias = `
INSERT INTO model_aliases (id, name, state, max_output_tokens, reservation_cap, created_at, updated_at, retired_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

const insertCandidate = `
INSERT INTO model_candidates (id, alias_id, position, backend_id, provider_model, parameter_overrides, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

const selectAlias = `
SELECT id, name, state, max_output_tokens, reservation_cap, created_at, updated_at, retired_at
FROM model_aliases
WHERE id = $1`

// ByName's lookup: the same row ByID reads, named the way requests name it —
// through the total unique index on name, so retired names resolve too and a
// miss is final.
const selectAliasByName = `
SELECT id, name, state, max_output_tokens, reservation_cap, created_at, updated_at, retired_at
FROM model_aliases
WHERE name = $1`

const selectCandidates = `
SELECT id, backend_id, provider_model, position, parameter_overrides
FROM model_candidates
WHERE alias_id = $1
ORDER BY position`

// The one-way move. retired_at is stamped in the same statement as the state
// flip, and the schema's model_aliases_retirement_consistency check would
// refuse the row if the two ever disagreed — the statement and the
// constraint say the same thing, which is what makes either of them
// trustworthy.
const retireAlias = `
UPDATE model_aliases
SET state = 'retired', retired_at = $2, updated_at = $2
WHERE id = $1 AND state = $3`

// The active-state guard SetCandidates and UpdateBounds travel behind: the
// write happens only while the row still reads the state the caller based
// its move on, so an edit racing a retirement loses cleanly and re-reads.
const guardAliasActive = `
UPDATE model_aliases
SET updated_at = $2
WHERE id = $1 AND state = $3`

const replaceCandidates = `
DELETE FROM model_candidates
WHERE alias_id = $1`

const updateAliasBounds = `
UPDATE model_aliases
SET max_output_tokens = $3, reservation_cap = $4, updated_at = $5
WHERE id = $1 AND state = $2`

func (r *aliasRepo) Create(ctx context.Context, alias *catalog.ModelAlias) error {
	var retiredAt any
	if alias.RetiredAt != nil {
		retiredAt = *alias.RetiredAt
	}
	if _, err := r.store.Querier(ctx).ExecContext(ctx, insertAlias,
		string(alias.ID), alias.Name, string(alias.State),
		alias.MaxOutputTokens, alias.ReservationCap,
		alias.CreatedAt, alias.UpdatedAt, retiredAt); err != nil {
		// On this insert a uniqueness violation can only be the name index:
		// the id is a fresh UUIDv7, so the primary key is not a realistic
		// second path to 23505. The index is total — retired names are
		// never reissued — so the sentinel is the collision's permanent
		// form, and the constraint name stays below the port, where it
		// belongs.
		if state, ok := sqlStateOf(err); ok && state == pgUniqueViolation {
			return fmt.Errorf("postgres: create alias %s: %w", alias.ID, catalog.ErrAliasNameTaken)
		}
		return fmt.Errorf("postgres: create alias %s: %w", alias.ID, err)
	}
	return r.insertCandidates(ctx, alias)
}

func (r *aliasRepo) ByID(ctx context.Context, id catalog.AliasID) (*catalog.ModelAlias, error) {
	return r.aliasOf(ctx, selectAlias, string(id), string(id))
}

func (r *aliasRepo) ByName(ctx context.Context, name string) (*catalog.ModelAlias, error) {
	return r.aliasOf(ctx, selectAliasByName, name, name)
}

// aliasOf reads one alias aggregate through query: the row scan, the state
// and retirement columns mapped back onto the domain's optional shapes, then
// the whole candidate list in position order — the same aggregate ByID and
// ByName promise, so the two lookups cannot drift into reading different
// shapes. label is what the errors name the lookup by (the id or the name);
// arg is the query's single parameter.
func (r *aliasRepo) aliasOf(ctx context.Context, query, arg, label string) (*catalog.ModelAlias, error) {
	var a catalog.ModelAlias
	var state string
	var retiredAt sql.NullTime
	err := r.store.Querier(ctx).QueryRowContext(ctx, query, arg).
		Scan(&a.ID, &a.Name, &state, &a.MaxOutputTokens, &a.ReservationCap, &a.CreatedAt, &a.UpdatedAt, &retiredAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("postgres: alias %s: %w", label, persistence.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: alias %s: %w", label, err)
	}
	a.State = catalog.AliasState(state)
	if retiredAt.Valid {
		t := retiredAt.Time
		a.RetiredAt = &t
	}
	candidates, err := r.candidatesOf(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	a.Candidates = candidates
	return &a, nil
}

func (r *aliasRepo) Retire(ctx context.Context, id catalog.AliasID, from catalog.AliasState, retiredAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, retireAlias, string(id), retiredAt, string(from))
	if err != nil {
		return false, fmt.Errorf("postgres: retire alias %s: %w", id, err)
	}
	return rowsApplied(res, "retire alias", id)
}

func (r *aliasRepo) SetCandidates(ctx context.Context, id catalog.AliasID, candidates []catalog.Candidate, updatedAt time.Time) (bool, error) {
	// The guard runs first and answers the CAS question; the replacement
	// only executes when the alias still reads active, and all three
	// statements ride the caller's unit of work, so a lost race leaves the
	// outgoing list standing exactly as it was.
	applied, err := r.guardActive(ctx, id, updatedAt)
	if err != nil || !applied {
		return applied, err
	}
	if _, err := r.store.Querier(ctx).ExecContext(ctx, replaceCandidates, string(id)); err != nil {
		return false, fmt.Errorf("postgres: set candidates on alias %s: clear outgoing list: %w", id, err)
	}
	return true, r.insertCandidates(ctx, aliasShell(id, candidates, updatedAt))
}

func (r *aliasRepo) UpdateBounds(ctx context.Context, id catalog.AliasID, from catalog.AliasState, maxOutputTokens, reservationCap int64, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, updateAliasBounds,
		string(id), string(from), maxOutputTokens, reservationCap, updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: update bounds on alias %s: %w", id, err)
	}
	return rowsApplied(res, "update bounds on alias", id)
}

// insertCandidates writes the aggregate's list at its assigned positions.
// It runs through the caller's Querier, so Create's alias row and these
// candidate rows commit together or not at all — the whole-aggregate rule
// the schema's foreign keys also enforce.
func (r *aliasRepo) insertCandidates(ctx context.Context, alias *catalog.ModelAlias) error {
	for _, c := range alias.Candidates {
		var overrides any
		if len(c.ParameterOverrides) > 0 {
			overrides = []byte(c.ParameterOverrides)
		}
		if _, err := r.store.Querier(ctx).ExecContext(ctx, insertCandidate,
			string(c.ID), string(alias.ID), c.Position, string(c.BackendID),
			c.ProviderModel, overrides, alias.CreatedAt); err != nil {
			return fmt.Errorf("postgres: insert candidate %d of alias %s: %w", c.Position, alias.ID, err)
		}
	}
	return nil
}

func (r *aliasRepo) candidatesOf(ctx context.Context, id catalog.AliasID) ([]catalog.Candidate, error) {
	rows, err := r.store.Querier(ctx).QueryContext(ctx, selectCandidates, string(id))
	if err != nil {
		return nil, fmt.Errorf("postgres: alias %s: list candidates: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []catalog.Candidate
	for rows.Next() {
		var c catalog.Candidate
		var backendID string
		var overrides []byte
		if err := rows.Scan(&c.ID, &backendID, &c.ProviderModel, &c.Position, &overrides); err != nil {
			return nil, fmt.Errorf("postgres: alias %s: scan candidate: %w", id, err)
		}
		c.BackendID = catalog.BackendID(backendID)
		if len(overrides) > 0 {
			c.ParameterOverrides = json.RawMessage(overrides)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: alias %s: list candidates: %w", id, err)
	}
	return candidates, nil
}

func (r *aliasRepo) guardActive(ctx context.Context, id catalog.AliasID, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, guardAliasActive,
		string(id), updatedAt, string(catalog.AliasActive))
	if err != nil {
		return false, fmt.Errorf("postgres: guard alias %s active: %w", id, err)
	}
	return rowsApplied(res, "guard alias", id)
}

// aliasShell is the carrier insertCandidates needs when the caller is a
// transition rather than a Create: the alias id, the successor list, and the
// stamp the candidates carry. Candidate rows are created_at-stamped by their
// writing transaction; a replaced list's rows are new rows with a new
// instant, which is what "replaced, not patched" means on disk.
func aliasShell(id catalog.AliasID, candidates []catalog.Candidate, createdAt time.Time) *catalog.ModelAlias {
	return &catalog.ModelAlias{ID: id, Candidates: candidates, CreatedAt: createdAt}
}

// ---------------------------------------------------------------------------
// alias_group_versions — version row plus member rows, one snapshot.
// ---------------------------------------------------------------------------

type groupVersionRepo struct {
	store persistence.Store
}

const insertGroupVersion = `
INSERT INTO alias_group_versions (id, group_name, version, created_at)
VALUES ($1, $2, $3, $4)`

const insertGroupMember = `
INSERT INTO alias_group_members (group_version_id, alias_id)
VALUES ($1, $2)`

const selectGroupVersion = `
SELECT id, group_name, version, created_at
FROM alias_group_versions
WHERE group_name = $1 AND version = $2`

const selectGroupMembers = `
SELECT alias_id
FROM alias_group_members
WHERE group_version_id = $1
ORDER BY alias_id`

const selectHighestGroupVersion = `
SELECT COALESCE(MAX(version), 0)
FROM alias_group_versions
WHERE group_name = $1`

func (r *groupVersionRepo) Create(ctx context.Context, version *catalog.AliasGroupVersion) error {
	if _, err := r.store.Querier(ctx).ExecContext(ctx, insertGroupVersion,
		string(version.ID), version.GroupName, version.Version, version.CreatedAt); err != nil {
		// A uniqueness violation here is the (group_name, version) index —
		// or, for the wildcard, the singleton index; both say "this
		// snapshot already exists", which is the open-version race's
		// signal to re-read and retry, not an operator-facing error.
		if state, ok := sqlStateOf(err); ok && state == pgUniqueViolation {
			return fmt.Errorf("postgres: create group version %s/%s@%d: %w", version.ID, version.GroupName, version.Version, catalog.ErrGroupVersionExists)
		}
		return fmt.Errorf("postgres: create group version %s/%s@%d: %w", version.ID, version.GroupName, version.Version, err)
	}
	for _, member := range version.Members {
		if _, err := r.store.Querier(ctx).ExecContext(ctx, insertGroupMember,
			string(version.ID), string(member)); err != nil {
			return fmt.Errorf("postgres: create group version %s: insert member %s: %w", version.ID, member, err)
		}
	}
	return nil
}

func (r *groupVersionRepo) ByGroupAndVersion(ctx context.Context, groupName string, version int) (*catalog.AliasGroupVersion, error) {
	var v catalog.AliasGroupVersion
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectGroupVersion, groupName, version).
		Scan(&v.ID, &v.GroupName, &v.Version, &v.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("postgres: group version %s@%d: %w", groupName, version, persistence.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: group version %s@%d: %w", groupName, version, err)
	}
	members, err := r.membersOf(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	v.Members = members
	return &v, nil
}

func (r *groupVersionRepo) HighestVersion(ctx context.Context, groupName string) (int, error) {
	var highest int
	if err := r.store.Querier(ctx).QueryRowContext(ctx, selectHighestGroupVersion, groupName).Scan(&highest); err != nil {
		return 0, fmt.Errorf("postgres: group %s: read highest version: %w", groupName, err)
	}
	return highest, nil
}

func (r *groupVersionRepo) membersOf(ctx context.Context, id catalog.GroupVersionID) ([]catalog.AliasID, error) {
	rows, err := r.store.Querier(ctx).QueryContext(ctx, selectGroupMembers, string(id))
	if err != nil {
		return nil, fmt.Errorf("postgres: group version %s: list members: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	members := []catalog.AliasID{}
	for rows.Next() {
		var member string
		if err := rows.Scan(&member); err != nil {
			return nil, fmt.Errorf("postgres: group version %s: scan member: %w", id, err)
		}
		members = append(members, catalog.AliasID(member))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: group version %s: list members: %w", id, err)
	}
	return members, nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// nullRef translates the domain's absence — the zero string — into the
// column's NULL. An empty string is not a reference value the schema would
// accept, and the domain never produces one; this is the round trip's other
// half.
func nullRef(ref string) any {
	if ref == "" {
		return nil
	}
	return ref
}

// rowsApplied reads a compare-and-swap's outcome: one row written means the
// caller's state was still current; zero means someone else moved first and
// the caller re-reads.
func rowsApplied(res sql.Result, what string, id any) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: %s %s: read rows affected: %w", what, id, err)
	}
	return n == 1, nil
}
