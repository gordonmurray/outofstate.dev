// Package tfstate reads Terraform state (format version 4) and extracts the
// identities of managed aws_db_instance resources.
//
// State contains secrets. This package decodes only an allowlist of
// attributes, never returns raw state, and never puts state content into
// error messages: errors are fixed categories (see Category).
package tfstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Category is a fixed failure class. Its message never contains state content.
type Category string

// Source-level failure categories produced by Parse.
const (
	CategoryNoSnapshot   Category = "no_snapshot"
	CategoryInvalidJSON  Category = "invalid_json"
	CategoryStateVersion Category = "state_version"
)

var categoryMessages = map[Category]string{
	CategoryNoSnapshot:   "no state snapshot exists in this workspace",
	CategoryInvalidJSON:  "output is not valid state JSON",
	CategoryStateVersion: "state is not format version 4",
}

// Error is returned by Parse. It carries only a Category.
type Error struct{ Category Category }

func (e *Error) Error() string { return categoryMessages[e.Category] }

// Lifecycle of a resource object in state.
type Lifecycle string

// Lifecycle values. Tainted and deposed objects still count as tracked.
const (
	LifecycleCurrent Lifecycle = "current"
	LifecycleTainted Lifecycle = "tainted"
	LifecycleDeposed Lifecycle = "deposed"
)

// Identity is one managed aws_db_instance object found in state, reduced to
// the fields needed for matching.
type Identity struct {
	Address       string // e.g. module.db["eu"].aws_db_instance.main[0]
	Lifecycle     Lifecycle
	DeposedKey    string
	SchemaVersion int

	ARN        string // validated RDS DB-instance ARN
	Identifier string // DB identifier (user-chosen name)
	ResourceID string // DbiResourceId, "db-..."

	Partition string // parsed from ARN
	Region    string
	AccountID string

	// From the Terraform resource identity (provider v6.62+, Terraform 1.12+),
	// when present. Evidence only; scope always comes from the ARN.
	IdentityAccountID  string
	IdentityRegion     string
	IdentityIdentifier string
}

// ProblemKind explains why an aws_db_instance object could not be used.
type ProblemKind string

// Problem kinds. Any problem makes the source partial.
const (
	ProblemMissingARN       ProblemKind = "missing_arn"
	ProblemInvalidARN       ProblemKind = "invalid_arn"
	ProblemUnreadableObject ProblemKind = "unreadable_object"
	// A resource whose mode, type or name could not be decoded. It might be
	// an aws_db_instance, so the source cannot be trusted as complete.
	ProblemUnreadableResource ProblemKind = "unreadable_resource"
	ProblemUnknownStatus      ProblemKind = "unknown_status"
)

// Problem names the resource address and the kind of problem, never values.
// Address is empty for ProblemUnreadableResource.
type Problem struct {
	Address string
	Kind    ProblemKind
}

// State is the parsed, reduced form of one state snapshot.
type State struct {
	Lineage    string
	Serial     int64
	Identities []Identity
	Problems   []Problem
}

// Partial reports whether some aws_db_instance objects could not be used.
func (s *State) Partial() bool { return len(s.Problems) > 0 }

const resourceType = "aws_db_instance"

// Raw state shapes. Only the fields oos needs are declared, so everything
// else (including secrets) is skipped by the decoder.
//
// Resources and instances are decoded one object at a time, so that one
// unexpected value only affects that object, not the whole snapshot.
type rawState struct {
	Version   *int              `json:"version"`
	Serial    int64             `json:"serial"`
	Lineage   string            `json:"lineage"`
	Resources []json.RawMessage `json:"resources"`
}

type rawResource struct {
	Module    string            `json:"module"`
	Mode      string            `json:"mode"`
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Instances []json.RawMessage `json:"instances"`
}

type rawInstance struct {
	IndexKey              json.RawMessage `json:"index_key"`
	Status                string          `json:"status"`
	Deposed               string          `json:"deposed"`
	SchemaVersion         int             `json:"schema_version"`
	Attributes            json.RawMessage `json:"attributes"`
	AttributesFlat        json.RawMessage `json:"attributes_flat"`
	Identity              json.RawMessage `json:"identity"`
	IdentitySchemaVersion int             `json:"identity_schema_version"`
}

type rawAttributes struct {
	ARN        string `json:"arn"`
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	ResourceID string `json:"resource_id"`
}

type rawIdentity struct {
	AccountID  string `json:"account_id"`
	Region     string `json:"region"`
	Identifier string `json:"identifier"`
}

// Parse reads one state snapshot, as printed by "terraform state pull" or
// stored in a state file.
func Parse(data []byte) (*State, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &Error{CategoryNoSnapshot}
	}
	var raw rawState
	if err := json.Unmarshal(data, &raw); err != nil {
		// The decoder's message is discarded: it is not shown anywhere.
		return nil, &Error{CategoryInvalidJSON}
	}
	if raw.Version == nil || *raw.Version != 4 {
		return nil, &Error{CategoryStateVersion}
	}

	st := &State{Lineage: raw.Lineage, Serial: raw.Serial}
	for _, rawRes := range raw.Resources {
		var res rawResource
		// JSON null and missing fields decode without error, so the required
		// fields are checked explicitly. Without them the resource cannot be
		// ruled out as an aws_db_instance.
		if err := json.Unmarshal(rawRes, &res); err != nil ||
			(res.Mode != "managed" && res.Mode != "data") || res.Type == "" || res.Name == "" {
			st.Problems = append(st.Problems, Problem{Kind: ProblemUnreadableResource})
			continue
		}
		if res.Mode != "managed" || res.Type != resourceType {
			continue
		}
		for _, rawInst := range res.Instances {
			var inst rawInstance
			if err := json.Unmarshal(rawInst, &inst); err != nil {
				st.Problems = append(st.Problems, Problem{Address: baseAddress(res), Kind: ProblemUnreadableObject})
				continue
			}
			addr, ok := address(res, inst.IndexKey)
			if !ok {
				st.Problems = append(st.Problems, Problem{Address: baseAddress(res), Kind: ProblemUnreadableObject})
				continue
			}
			id, kind := identity(addr, inst)
			if kind != "" {
				st.Problems = append(st.Problems, Problem{Address: addr, Kind: kind})
				continue
			}
			st.Identities = append(st.Identities, id)
		}
	}
	return st, nil
}

func identity(addr string, inst rawInstance) (Identity, ProblemKind) {
	attrs, ok := readAttributes(inst)
	if !ok {
		return Identity{}, ProblemUnreadableObject
	}

	id := Identity{Address: addr, SchemaVersion: inst.SchemaVersion, Lifecycle: LifecycleCurrent}
	switch inst.Status {
	case "":
	case "tainted":
		id.Lifecycle = LifecycleTainted
	default:
		return Identity{}, ProblemUnknownStatus
	}
	if inst.Deposed != "" {
		id.Lifecycle, id.DeposedKey = LifecycleDeposed, inst.Deposed
	}

	normalise(&id, attrs)

	// Only identity schema version 0 (the current aws_db_instance identity:
	// account_id, region, identifier) is understood. Others are ignored.
	if inst.IdentitySchemaVersion == 0 && len(inst.Identity) > 0 && string(inst.Identity) != "null" {
		var ri rawIdentity
		if json.Unmarshal(inst.Identity, &ri) == nil {
			id.IdentityAccountID, id.IdentityRegion, id.IdentityIdentifier = ri.AccountID, ri.Region, ri.Identifier
		}
	}

	if attrs.ARN == "" {
		return Identity{}, ProblemMissingARN
	}
	arn, ok := ParseARN(attrs.ARN)
	if !ok {
		return Identity{}, ProblemInvalidARN
	}
	id.ARN = attrs.ARN
	id.Partition, id.Region, id.AccountID = arn.Partition, arn.Region, arn.AccountID
	return id, ""
}

// normalise applies the provider-version rules from the plan (§6, §15):
// schema_version 2 (provider v5+) stores DbiResourceId in "id";
// schema_version 0/1 (provider v4) stores the DB identifier in "id".
// For any other schema version "id" is not interpreted at all. Explicit
// "resource_id" and "identifier" attributes always win. A "db-" prefix is
// never used as evidence.
func normalise(id *Identity, a rawAttributes) {
	id.ResourceID = a.ResourceID
	id.Identifier = a.Identifier

	switch id.SchemaVersion {
	case 2:
		if id.ResourceID == "" && a.ID != "" && a.ID != a.Identifier {
			id.ResourceID = a.ID
		}
	case 0, 1:
		if id.Identifier == "" {
			id.Identifier = a.ID
		}
	}
}

func readAttributes(inst rawInstance) (rawAttributes, bool) {
	var a rawAttributes
	if len(inst.Attributes) > 0 && string(inst.Attributes) != "null" {
		if err := json.Unmarshal(inst.Attributes, &a); err != nil {
			return a, false
		}
		return a, true
	}
	if len(inst.AttributesFlat) > 0 && string(inst.AttributesFlat) != "null" {
		// Flat attributes are a string map with the same top-level keys.
		// Decoding into the allowlisted struct skips every other key.
		if err := json.Unmarshal(inst.AttributesFlat, &a); err != nil {
			return a, false
		}
		return a, true
	}
	return a, false
}

func baseAddress(res rawResource) string {
	addr := res.Type + "." + res.Name
	if res.Module != "" {
		addr = res.Module + "." + addr
	}
	return addr
}

// address builds the resource instance address. Integer keys render as [0];
// string keys render quoted and escaped, as Terraform prints them.
func address(res rawResource, key json.RawMessage) (string, bool) {
	base := baseAddress(res)
	k := bytes.TrimSpace(key)
	if len(k) == 0 || string(k) == "null" {
		return base, true
	}
	switch k[0] {
	case '"':
		var s string
		if err := json.Unmarshal(k, &s); err != nil {
			return "", false
		}
		return base + "[" + quote(s) + "]", true
	default:
		var n json.Number
		if err := json.Unmarshal(k, &n); err != nil {
			return "", false
		}
		i, err := n.Int64()
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("%s[%d]", base, i), true
	}
}

// quote renders a string index key the way Terraform prints it in
// addresses, using HCL quoted-string escaping: the usual backslash escapes,
// doubled template introducers ("${" becomes "$${", "%{" becomes "%%{"),
// and \uXXXX / \UXXXXXXXX for non-printable characters.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i, r := range s {
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '$', '%':
			b.WriteRune(r)
			if strings.HasPrefix(s[i+1:], "{") {
				b.WriteRune(r)
			}
		default:
			switch {
			case unicode.IsPrint(r):
				b.WriteRune(r)
			case r < 0x10000:
				fmt.Fprintf(&b, "\\u%04x", r)
			default:
				fmt.Fprintf(&b, "\\U%08x", r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
