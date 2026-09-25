package projection

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// projectionContractPath is the fragment this package speaks, relative to this
// package's directory — `go test` runs each package with that directory as its
// working directory. The five levels back are: projection, domain, internal,
// console-api, apps.
//
// The projection's agreement cannot be typed across the planes: the Data Plane
// is another module and judges every message against this document's schemas,
// while this package builds every message from its own constructors. Reading
// the document here is what makes that arrangement safe — a bound widened in
// the yaml and left narrow in the constants, or the reverse, delivers messages
// one side always refuses, and every behaviour test in this package would stay
// green through the drift because they all exercise the same Go values. This
// file measures the code against the document instead, so a contract edit and
// a grammar edit must travel together.
const projectionContractPath = "../../../../../api/openapi/shared/projection.yaml"

// TestTheContractSpeaksTheProtocolVersionThisPackagePins ties the fragment's
// version to the constant every message this package renders carries.
//
// The versioning rule fails every hop closed against a version it does not
// know, so a build whose document says one number and whose constructors write
// another produces messages that are refused on arrival — loudly, but by the
// far side of the seam rather than by the test suite that could have caught
// it. The literal 1 is pinned beside the constant because a new version is a
// reviewed contract change with a migration of every hop, not a number to
// edit and ship.
func TestTheContractSpeaksTheProtocolVersionThisPackagePins(t *testing.T) {
	const declared = "components.schemas.ProjectionProtocolVersion.const"
	version, ok := scanContract(t, projectionContractPath).numbers[declared]
	if !ok {
		t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", projectionContractPath, declared)
	}
	if version != ProtocolVersion {
		t.Errorf("%s says the protocol version is %d and this package pins %d; every hop fails closed against a version it does not know, so the disagreement delivers messages refused on arrival", projectionContractPath, version, ProtocolVersion)
	}
	if ProtocolVersion != 1 {
		t.Errorf("ProtocolVersion = %d, want 1; the fragment spells the version as the literal 1, and a new one is a reviewed contract change with a migration of every hop, not a constant to edit", ProtocolVersion)
	}
}

// TestTheContractBoundsAndFloorsAreWhatThisPackageEnforces pins every number
// the fragment declares to the number this package's grammar enforces.
//
// Each row carries the literal twice on purpose: once as the value the
// document must spell, once as the value the code must enforce. The bounds are
// shared between a producer that batches at them and a consumer that refuses
// past them, and the floors describe what a hop may name a revision or a
// boundary at all; a build whose two copies disagree produces messages one
// side always refuses, and the behaviour tests never notice because they
// exercise the constant the check uses.
func TestTheContractBoundsAndFloorsAreWhatThisPackageEnforces(t *testing.T) {
	numbers := scanContract(t, projectionContractPath).numbers

	tests := []struct {
		name              string
		key               string
		enforced          int // what this package's constructors enforce
		spelledInDocument int // the literal the document must spell
	}{
		{name: "the version every message carries", key: "components.schemas.ProjectionProtocolVersion.const", enforced: ProtocolVersion, spelledInDocument: 1},
		{name: "the most changes one batch may carry", key: "components.schemas.ProjectionChangesBatch.properties.changes.maxItems", enforced: MaxChangesPerBatch, spelledInDocument: 200},
		{name: "the most credentials one snapshot may carry", key: "components.schemas.ProjectionSnapshot.properties.api_keys.maxItems", enforced: MaxSnapshotKeys, spelledInDocument: 5000},
		{name: "the most accounts one snapshot may carry", key: "components.schemas.ProjectionSnapshot.properties.accounts.maxItems", enforced: MaxSnapshotAccounts, spelledInDocument: 5000},
		{name: "the length of every identifier the contract calls a uuid", key: "components.schemas.ProjectionEpoch.maxLength", enforced: 36, spelledInDocument: 36},
		{name: "the revision the change counter allocates from", key: "components.schemas.ProjectionChange.properties.revision.minimum", enforced: 1, spelledInDocument: 1},
		{name: "the boundary a fresh timeline's snapshot is cut at", key: "components.schemas.ProjectionSnapshot.properties.snapshot_revision.minimum", enforced: 0, spelledInDocument: 0},
		{name: "the position a delivery may start from before any snapshot", key: "components.schemas.ProjectionChangesBatch.properties.from_revision.minimum", enforced: 0, spelledInDocument: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared, ok := numbers[tt.key]
			if !ok {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", projectionContractPath, tt.key)
			}
			if declared != tt.enforced {
				t.Errorf("%s says %s is %d and this package enforces %d; the code and the contract have drifted, and every behaviour test in this package stays green through the drift because they exercise the same constant", projectionContractPath, tt.key, declared, tt.enforced)
			}
			if tt.enforced != tt.spelledInDocument {
				t.Errorf("this package enforces %d where the document must spell %d for %s; the literal is kept here so the drift message can name the number the contract declares", tt.enforced, tt.spelledInDocument, tt.key)
			}
		})
	}
}

// TestTheContractRecordsAreTheEnvelopesThisPackageRenders pins the field names
// of every message this package renders to the `required` lists the fragment
// declares.
//
// A rendering is where a renamed field is most quietly wrong: a tag that moved
// with its struct would still marshal, and every behaviour test written
// against that struct would still pass, while the consumer decoded a zero
// value — or refused the object outright, where the schema closes it. What
// follows marshals the real wire bytes and compares their whole key sets
// against the document, so the one file both planes claim to implement is the
// thing the renderers are measured against.
func TestTheContractRecordsAreTheEnvelopesThisPackageRenders(t *testing.T) {
	document := scanContract(t, projectionContractPath)

	changeJSON, err := json.Marshal(mustCredentialChange(t, 1, mustCredential(t)))
	if err != nil {
		t.Fatalf("marshalling an api_key change: %v", err)
	}
	batchJSON, err := json.Marshal(mustBatch(t, 0, 1))
	if err != nil {
		t.Fatalf("marshalling a batch: %v", err)
	}
	snapshotJSON, err := json.Marshal(mustSnapshot(t, 0, syntheticKeys(t, 1), syntheticAccounts(t, 1)))
	if err != nil {
		t.Fatalf("marshalling a snapshot: %v", err)
	}

	tests := []struct {
		name       string
		schema     string
		fieldNames []string
	}{
		{name: "the credential record", schema: "components.schemas.ApiKeyCredential.required", fieldNames: sortedKeys(snapshotRecord(t, snapshotJSON, "api_keys", 0))},
		{name: "the account record", schema: "components.schemas.AccountState.required", fieldNames: sortedKeys(snapshotRecord(t, snapshotJSON, "accounts", 0))},
		{name: "the change envelope", schema: "components.schemas.ProjectionChange.required", fieldNames: sortedKeys(decodedObject(t, changeJSON))},
		{name: "the batch envelope", schema: "components.schemas.ProjectionChangesBatch.required", fieldNames: sortedKeys(decodedObject(t, batchJSON))},
		{name: "the snapshot envelope", schema: "components.schemas.ProjectionSnapshot.required", fieldNames: sortedKeys(decodedObject(t, snapshotJSON))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared := document.lists[tt.schema]
			if len(declared) == 0 {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", projectionContractPath, tt.schema)
			}
			slices.Sort(declared)
			if !slices.Equal(tt.fieldNames, declared) {
				t.Errorf("this package renders %v and %s requires %v; a field on one side and not the other decodes to a zero value at the consumer, or is refused whole where the schema closes the object", tt.fieldNames, projectionContractPath, declared)
			}
		})
	}
}

// TestAChangePayloadIsItsRecordMinusTheIdentityTheEnvelopeCarries pins the
// payload's field names to the only structural statement the fragment makes
// about them. The payload itself is left open in the schema — unknown fields
// inside it are the one direction this protocol evolves without a version bump
// — and its fields are described in prose: for `api_key` the record's fields
// less the key id, which the envelope carries instead; for `account` the
// record's fields less the account id, for the same reason. This pin ties that
// prose to the renderers, so a property added to a record in the document
// turns red here until the payload that mirrors the row carries it too.
func TestAChangePayloadIsItsRecordMinusTheIdentityTheEnvelopeCarries(t *testing.T) {
	document := scanContract(t, projectionContractPath)

	changeJSON, err := json.Marshal(mustCredentialChange(t, 1, mustCredential(t)))
	if err != nil {
		t.Fatalf("marshalling an api_key change: %v", err)
	}
	accountChangeJSON, err := json.Marshal(mustAccountChange(t, 1, mustAccount(t)))
	if err != nil {
		t.Fatalf("marshalling an account change: %v", err)
	}

	tests := []struct {
		name         string
		recordSchema string
		identity     string
		changeJSON   []byte
	}{
		{name: "the api_key payload", recordSchema: "components.schemas.ApiKeyCredential.properties", identity: "key_id", changeJSON: changeJSON},
		{name: "the account payload", recordSchema: "components.schemas.AccountState.properties", identity: "account_id", changeJSON: accountChangeJSON},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared := document.children[tt.recordSchema]
			if len(declared) == 0 {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", projectionContractPath, tt.recordSchema)
			}
			slices.Sort(declared)
			want := slices.DeleteFunc(slices.Clone(declared), func(name string) bool { return name == tt.identity })

			payload, ok := decodedObject(t, tt.changeJSON)["payload"].(map[string]any)
			if !ok {
				t.Fatalf("the change rendered %v for `payload`, want an object; the payload is the row's whole state and nothing else can carry it", decodedObject(t, tt.changeJSON)["payload"])
			}
			if got := sortedKeys(payload); !slices.Equal(got, want) {
				t.Errorf("%s carries %v, want the %s record's fields %v less the identity %q the envelope carries; a field the payload omits is a fact the mirror row never learns", tt.name, got, tt.name, want, tt.identity)
			}
		})
	}
}

// TestTheContractEnumsAreTheStatesAndKindsThisPackageSpeaks pins the closed
// vocabularies. This package restates the contract's states rather than
// importing the ownership domain's, and deliberately so: a new projected state
// is a new protocol version and a reviewed migration of every hop, while an
// ownership state is an internal affair of the aggregate that owns it. The
// restatement is safe exactly while its spellings match the document, which is
// what this pin holds — a value spoken here and not declared there is a
// message no hop can route, and a value declared there and not spoken here is
// a state the producer can never project.
func TestTheContractEnumsAreTheStatesAndKindsThisPackageSpeaks(t *testing.T) {
	lists := scanContract(t, projectionContractPath).lists

	tests := []struct {
		name   string
		key    string
		spoken []string
	}{
		{name: "the credential states", key: "components.schemas.ApiKeyCredential.properties.state.enum", spoken: []string{string(CredentialActive), string(CredentialRevoked)}},
		{name: "the account states", key: "components.schemas.AccountState.properties.state.enum", spoken: []string{string(AccountActive), string(AccountSuspended), string(AccountClosed)}},
		{name: "the resource kinds", key: "components.schemas.ProjectionChange.properties.resource_kind.enum", spoken: []string{string(KindAPIKey), string(KindAccount)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared, ok := lists[tt.key]
			if !ok || len(declared) == 0 {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", projectionContractPath, tt.key)
			}
			slices.Sort(declared)
			spoken := slices.Clone(tt.spoken)
			slices.Sort(spoken)
			if !slices.Equal(declared, spoken) {
				t.Errorf("%s declares %v and this package speaks %v; the states here are the contract's, spelled as the wire spells them, so a value on one side and not the other is a message no hop can route", projectionContractPath, declared, spoken)
			}
		})
	}
}

// TestTheContractDigestPatternIsTheDigestGrammarThisPackageEnforces ties the
// fragment's one line of digest grammar to the check beside it in Go.
//
// The pattern is the strongest form the contract pins, and the database's own
// CHECKs repeat it on both stored copies of the log. NewDigest must enforce
// exactly that form: a digest outside the pattern would be refused by the
// consumer and by the mirror row, three hops after the code that minted it,
// and a pattern loosened in the document alone would admit a rendering no
// stored column accepts.
func TestTheContractDigestPatternIsTheDigestGrammarThisPackageEnforces(t *testing.T) {
	const key = "components.schemas.ApiKeyCredential.properties.digest.pattern"
	pattern, ok := scanContract(t, projectionContractPath).quoted[key]
	if !ok {
		t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", projectionContractPath, key)
	}
	if pattern != "^[0-9a-f]{64}$" {
		t.Errorf("%s declares the digest pattern %q, want ^[0-9a-f]{64}$; the document and NewDigest must describe one form of the secret's only transitable rendering", projectionContractPath, pattern)
	}
	if digestHexLength != 64 {
		t.Errorf("digestHexLength = %d, want 64; that is the {64} the document's pattern spells, two characters per byte of SHA-256", digestHexLength)
	}
}

// contractDocument is what scanning the fragment yields: the integer scalars
// and quoted scalars it declares, the child keys of every mapping, and the
// entries of every sequence — each keyed by dotted path, so a `maxItems` under
// one schema and a `maxItems` under another are two different keys and neither
// is confused for the other.
type contractDocument struct {
	numbers  map[string]int
	quoted   map[string]string
	children map[string][]string
	lists    map[string][]string
}

// scanContract reads the fragment's indentation-nested keys.
//
// It is a scanner rather than a parser because Go's standard library has no
// YAML parser and the shape being read is narrow: indentation-nested mapping
// keys and flat scalar sequences, in a document this repository writes by
// hand. Keys are pushed and popped by indentation, so `enum` under one schema
// and `enum` under another are two different paths; sequence entries attach to
// the innermost open key, which is how a `required:` list and an `enum:` list
// each arrive whole; and folded description prose falls out on its own, since
// a line is only read as a key when it reads exactly as `key: value` with no
// whitespace inside the key. A document that stopped matching the scan yields
// nothing, and every caller above treats an empty result as a failure rather
// than a skip, so the scan going blind is loud instead of vacuous.
func scanContract(t *testing.T, path string) contractDocument {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	document := contractDocument{
		numbers:  map[string]int{},
		quoted:   map[string]string{},
		children: map[string][]string{},
		lists:    map[string][]string{},
	}
	type frame struct {
		indent int
		path   string
	}
	stack := []frame{}

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if strings.HasPrefix(trimmed, "- ") {
			if len(stack) > 0 {
				item := strings.Trim(strings.TrimPrefix(trimmed, "- "), `"`)
				document.lists[stack[len(stack)-1].path] = append(document.lists[stack[len(stack)-1].path], item)
			}
			continue
		}

		key, value, found := strings.Cut(trimmed, ":")
		if !found || key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		value = strings.TrimSpace(value)

		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := ""
		if len(stack) > 0 {
			parent = stack[len(stack)-1].path
		}
		joined := key
		if parent != "" {
			joined = parent + "." + key
		}
		document.children[parent] = append(document.children[parent], key)

		if value == "" {
			// A mapping whose children follow at a deeper indent.
			stack = append(stack, frame{indent: indent, path: joined})
			continue
		}
		if number, err := strconv.Atoi(value); err == nil {
			document.numbers[joined] = number
			continue
		}
		if unquoted := strings.Trim(value, `"`); unquoted != value {
			document.quoted[joined] = unquoted
		}
	}

	if len(document.numbers) == 0 || len(document.children) == 0 {
		t.Fatalf("%s yielded no keys; every pin against it would prove nothing", path)
	}
	return document
}
