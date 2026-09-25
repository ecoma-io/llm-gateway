package catalog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewGroupVersionStoresTheNamedSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	id, err := NewGroupVersionID()
	if err != nil {
		t.Fatalf("NewGroupVersionID returned error: %v", err)
	}
	members := []AliasID{"alias-1", "alias-2", "alias-3"}
	version, err := NewGroupVersion(id, " frontier ", 4, members, now)
	if err != nil {
		t.Fatalf("NewGroupVersion returned error: %v", err)
	}
	if version.GroupName != "frontier" {
		t.Errorf("GroupName = %q, want the trimmed value", version.GroupName)
	}
	if version.Version != 4 {
		t.Errorf("Version = %d, want the caller's next number", version.Version)
	}
	// The snapshot is stored exactly as the operator named it — sorting the
	// set would be the catalog quietly rewriting an operator's scope.
	for i := range members {
		if version.Members[i] != members[i] {
			t.Errorf("member %d = %q, want %q — the snapshot is stored as given", i, version.Members[i], members[i])
		}
	}
	if version.IsWildcard() {
		t.Errorf("IsWildcard() = true for a named group")
	}
	if !version.Contains("alias-2") {
		t.Errorf("Contains(alias-2) = false inside its own snapshot")
	}
	if version.Contains("alias-9") {
		t.Errorf("Contains(alias-9) = true outside the snapshot")
	}
}

func TestNewGroupVersionRefusesAnIllFormedSnapshot(t *testing.T) {
	id, err := NewGroupVersionID()
	if err != nil {
		t.Fatalf("NewGroupVersionID returned error: %v", err)
	}
	for name, tc := range map[string]struct {
		group   string
		version int
		members []AliasID
		want    error
	}{
		"reserved name":        {"*", 1, []AliasID{"alias-1"}, ErrInvalidGroupName},
		"blank name":           {"", 1, []AliasID{"alias-1"}, ErrInvalidGroupName},
		"outside the grammar":  {"frontier group", 1, []AliasID{"alias-1"}, ErrInvalidGroupName},
		"version zero":         {"frontier", 0, []AliasID{"alias-1"}, ErrInvalidGroupName},
		"version negative":     {"frontier", -3, []AliasID{"alias-1"}, ErrInvalidGroupName},
		"empty membership":     {"frontier", 1, nil, ErrInvalidGroupMembers},
		"blank member":         {"frontier", 1, []AliasID{"alias-1", ""}, ErrInvalidGroupMembers},
		"duplicate membership": {"frontier", 1, []AliasID{"alias-1", "alias-2", "alias-1"}, ErrInvalidGroupMembers},
	} {
		_, err := NewGroupVersion(id, tc.group, tc.version, tc.members, time.Now())
		if err == nil {
			t.Fatalf("%s: NewGroupVersion accepted the snapshot", name)
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
}

func TestTheWildcardIsItsOwnSingleton(t *testing.T) {
	id, err := NewGroupVersionID()
	if err != nil {
		t.Fatalf("NewGroupVersionID returned error: %v", err)
	}
	wildcard, err := NewWildcardGroupVersion(id, time.Now())
	if err != nil {
		t.Fatalf("NewWildcardGroupVersion returned error: %v", err)
	}
	if wildcard.GroupName != GroupWildcardName {
		t.Errorf("GroupName = %q, want the reserved name", wildcard.GroupName)
	}
	// Version 1 is not a choice: the singleton's row is fully determined,
	// which is what the schema's wildcard CHECK pins too.
	if wildcard.Version != 1 {
		t.Errorf("Version = %d, want 1", wildcard.Version)
	}
	if len(wildcard.Members) != 0 {
		t.Errorf("Members = %v, want the empty set — every-alias needs no rows", wildcard.Members)
	}
	// The membership predicate: the wildcard contains everything, by
	// definition — that is the whole of what it is.
	if !wildcard.Contains("anything-at-all") || !wildcard.Contains("") {
		t.Errorf("Contains refused a member of everything")
	}
	// And a named group can never claim the reserved name through the
	// named-group path.
	if _, err := NewGroupVersion(id, GroupWildcardName, 2, []AliasID{"alias-1"}, time.Now()); !errors.Is(err, ErrInvalidGroupName) {
		t.Errorf("NewGroupVersion(%q) error = %v, want ErrInvalidGroupName", GroupWildcardName, err)
	}
}

func TestGroupNamesShareTheAliasGrammar(t *testing.T) {
	id, err := NewGroupVersionID()
	if err != nil {
		t.Fatalf("NewGroupVersionID returned error: %v", err)
	}
	for name, group := range map[string]string{
		"too long": strings.Repeat("g", 129),
		"unicode":  "nhóm-frontier",
	} {
		if _, err := NewGroupVersion(id, group, 1, []AliasID{"alias-1"}, time.Now()); err == nil {
			t.Errorf("%s: NewGroupVersion accepted %q", name, group)
		}
	}
}
