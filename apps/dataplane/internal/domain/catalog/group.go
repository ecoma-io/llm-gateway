package catalog

import (
	"fmt"
	"slices"
	"time"
)

// GroupWildcardName is the reserved group name whose membership is "every
// alias". It is not a name an operator may also use for a named group, and
// it is not built through the named-group path: the wildcard is one row,
// ever, at version 1, with no member rows — every alias by definition.
const GroupWildcardName = "*"

// AliasGroupVersion is one immutable membership snapshot of a named alias
// group (ADR 0003). It is the catalog's contract with the other plane:
// entitlements in the control database store a GroupVersionID and nothing
// else (ADR 0006 §7), so a version, once issued, never changes — a
// membership edit opens version n+1, grants issued earlier keep the
// snapshot they were granted with, and no read anywhere can observe a
// version's membership change under it.
//
// The snapshot is stored, not computed: the version's Members slice is the
// authoritative set, persisted as the version's member rows. Membership is
// never expanded into per-alias grants anywhere downstream — admission
// evaluates against this stored set (ADR 0003), which is why the set itself
// must be exactly what the operator named, duplicates and emptiness
// included in the refusal.
type AliasGroupVersion struct {
	ID        GroupVersionID
	GroupName string
	Version   int
	Members   []AliasID
	CreatedAt time.Time
}

// IsWildcard reports whether this version is the wildcard group's singleton.
// The wildcard's membership is "every alias" — encoded as the empty member
// set, because the set that contains everything is the one set that needs no
// rows — and its identity is the reserved name plus version 1, together the
// schema's single permitted wildcard row.
func (v *AliasGroupVersion) IsWildcard() bool {
	return v.GroupName == GroupWildcardName
}

// Contains reports whether the snapshot names alias id. On the wildcard it
// is always true — that is what the wildcard is. On a named group it is a
// membership test over the stored snapshot. This is the predicate the
// admission waterfall will one day run against every matching entitlement's
// scope; it lives on the aggregate because the stored set, not the caller,
// decides what it contains.
func (v *AliasGroupVersion) Contains(id AliasID) bool {
	if v.IsWildcard() {
		return true
	}
	return slices.Contains(v.Members, id)
}

// NewWildcardGroupVersion returns the wildcard group's one version: reserved
// name, version 1, empty member set. There is no version parameter and no
// member parameter on purpose — both are fixed by what the wildcard is, and
// the schema's partial unique index backs the "one, ever" with a constraint
// rather than a convention.
func NewWildcardGroupVersion(id GroupVersionID, now time.Time) (*AliasGroupVersion, error) {
	if id == "" {
		return nil, fmt.Errorf("catalog: new wildcard group version: blank id")
	}
	return &AliasGroupVersion{
		ID:        id,
		GroupName: GroupWildcardName,
		Version:   1,
		Members:   []AliasID{},
		CreatedAt: now.UTC(),
	}, nil
}

// NewGroupVersion returns a named group's next snapshot: the operator's
// member list, stored exactly as given. The rules the snapshot must satisfy
// are the snapshot's whole integrity: a non-empty member set (an empty
// snapshot is a scope that grants nothing, which is the wildcard's job to
// negate, not a named group's to mimic), no duplicate members (one alias's
// containment must not be ambiguous), and the shared name grammar with `*`
// reserved. The version number is supplied by the use case — it reads the
// group's highest version and passes version+1, so callers never invent one
// — and it must be at least 1.
func NewGroupVersion(id GroupVersionID, groupName string, version int, members []AliasID, now time.Time) (*AliasGroupVersion, error) {
	groupName = trimSpace(groupName)
	if groupName == GroupWildcardName {
		// The wildcard has its own constructor and its one row; a named
		// group claiming the reserved name would be a second wildcard by
		// another door.
		return nil, fmt.Errorf("catalog: new group version: %w: %q is reserved for the wildcard", ErrInvalidGroupName, GroupWildcardName)
	}
	if !aliasNamePattern.MatchString(groupName) {
		return nil, fmt.Errorf("catalog: new group version: %w: %q is outside the group-name grammar", ErrInvalidGroupName, groupName)
	}
	if version < 1 {
		return nil, fmt.Errorf("catalog: new group version: %w: version %d is below 1", ErrInvalidGroupName, version)
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("catalog: new group version: %w: member set is empty", ErrInvalidGroupMembers)
	}
	if id == "" {
		return nil, fmt.Errorf("catalog: new group version: blank id")
	}
	seen := make(map[AliasID]bool, len(members))
	for _, member := range members {
		if member == "" {
			return nil, fmt.Errorf("catalog: new group version: %w: blank member id", ErrInvalidGroupMembers)
		}
		if seen[member] {
			return nil, fmt.Errorf("catalog: new group version: %w: alias %s listed twice", ErrInvalidGroupMembers, member)
		}
		seen[member] = true
	}
	return &AliasGroupVersion{
		ID:        id,
		GroupName: groupName,
		Version:   version,
		Members:   slices.Clone(members),
		CreatedAt: now.UTC(),
	}, nil
}
