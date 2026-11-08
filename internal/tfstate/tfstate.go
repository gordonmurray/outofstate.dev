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
	"slices"
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

// Identity is one managed object of a supported type found in state,
// reduced to the fields needed for matching.
type Identity struct {
	Type          string // aws_db_instance or aws_lb (aws_alb is recorded as aws_lb)
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
	ProblemMissingID        ProblemKind = "missing_id"
	ProblemInvalidID        ProblemKind = "invalid_id"
	ProblemUnreadableObject ProblemKind = "unreadable_object"
	// A resource whose mode, type or name could not be decoded. It might be
	// an aws_db_instance, so the source cannot be trusted as complete.
	ProblemUnreadableResource ProblemKind = "unreadable_resource"
	ProblemUnknownStatus      ProblemKind = "unknown_status"
)

// Problem names the resource address, its type and the kind of problem,
// never values.
// Address is empty for ProblemUnreadableResource.
type Problem struct {
	Address string
	Type    string // recorded type (aws_lb for aws_alb); empty when unknown
	Kind    ProblemKind
}

// Keep reduces the state to the given recorded types: identities and
// problems of other types are dropped. Problems whose type is unknown
// (unreadable resources) are always kept, because they could be any type.
func (s *State) Keep(types []string) {
	var ids []Identity
	for _, id := range s.Identities {
		if slices.Contains(types, id.Type) {
			ids = append(ids, id)
		}
	}
	var probs []Problem
	for _, p := range s.Problems {
		if p.Type == "" || slices.Contains(types, p.Type) {
			probs = append(probs, p)
		}
	}
	s.Identities, s.Problems = ids, probs
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

// Supported resource types and the type oos records for each. aws_alb is the
// older name of aws_lb and means the same resource.
var supportedTypes = map[string]string{
	"aws_db_instance": "aws_db_instance",
	"aws_lb":          "aws_lb",
	"aws_alb":         "aws_lb",
	"aws_s3_bucket":   "aws_s3_bucket",

	"aws_lambda_function": "aws_lambda_function",
	"aws_nat_gateway":     "aws_nat_gateway",
}

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
	Name       string `json:"name"`   // aws_lb
	Bucket     string `json:"bucket"` // aws_s3_bucket
	Region     string `json:"region"` // aws_s3_bucket, aws_nat_gateway
	Identifier string `json:"identifier"`
	ResourceID string `json:"resource_id"`
}

type rawIdentity struct {
	AccountID  string `json:"account_id"`
	Region     string `json:"region"`
	Identifier string `json:"identifier"`
	Bucket     string `json:"bucket"`
	ID         string `json:"id"` // aws_nat_gateway
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
		typ, supported := supportedTypes[res.Type]
		if res.Mode != "managed" || !supported {
			continue
		}
		for _, rawInst := range res.Instances {
			var inst rawInstance
			if err := json.Unmarshal(rawInst, &inst); err != nil {
				st.Problems = append(st.Problems, Problem{Address: baseAddress(res), Type: typ, Kind: ProblemUnreadableObject})
				continue
			}
			addr, ok := address(res, inst.IndexKey)
			if !ok {
				st.Problems = append(st.Problems, Problem{Address: baseAddress(res), Type: typ, Kind: ProblemUnreadableObject})
				continue
			}
			id, kind := identity(typ, addr, inst)
			if kind != "" {
				st.Problems = append(st.Problems, Problem{Address: addr, Type: typ, Kind: kind})
				continue
			}
			st.Identities = append(st.Identities, id)
		}
	}
	return st, nil
}

func identity(typ, addr string, inst rawInstance) (Identity, ProblemKind) {
	attrs, ok := readAttributes(inst)
	if !ok {
		return Identity{}, ProblemUnreadableObject
	}

	id := Identity{Type: typ, Address: addr, SchemaVersion: inst.SchemaVersion, Lifecycle: LifecycleCurrent}
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

	switch typ {
	case "aws_lb":
		return lbIdentity(id, attrs)
	case "aws_s3_bucket":
		ri, _ := readIdentity(inst)
		return bucketIdentity(id, attrs, ri)
	case "aws_lambda_function":
		return lambdaIdentity(id, attrs)
	case "aws_nat_gateway":
		ri, ok := readIdentity(inst)
		if !ok {
			return Identity{}, ProblemUnreadableObject
		}
		return natIdentity(id, attrs, ri)
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

// lbIdentity reads an aws_lb (or aws_alb) object. Its identity is the load
// balancer ARN, which ends in a generated suffix, so a load balancer recreated
// with the same name has a different ARN. In state, "id" holds the ARN too.
func lbIdentity(id Identity, a rawAttributes) (Identity, ProblemKind) {
	arnStr := a.ARN
	if arnStr == "" {
		arnStr = a.ID
	}
	if arnStr == "" {
		return Identity{}, ProblemMissingARN
	}
	arn, ok := ParseLBARN(arnStr)
	if !ok {
		return Identity{}, ProblemInvalidARN
	}
	id.ARN = arnStr
	id.Identifier = a.Name
	if id.Identifier == "" {
		id.Identifier = arn.Name
	}
	id.Partition, id.Region, id.AccountID = arn.Partition, arn.Region, arn.AccountID
	return id, ""
}

// readIdentity decodes the Terraform resource identity. Only identity schema
// version 0 is understood; anything else, or no identity, gives an empty one.
// It returns false when an identity is present but cannot be decoded.
func readIdentity(inst rawInstance) (rawIdentity, bool) {
	var ri rawIdentity
	if inst.IdentitySchemaVersion == 0 && len(inst.Identity) > 0 && string(inst.Identity) != "null" {
		if err := json.Unmarshal(inst.Identity, &ri); err != nil {
			return ri, false
		}
	}
	return ri, true
}

// natIdentity reads an aws_nat_gateway object. Its identity is the gateway ID
// in "id". The resource has no ARN, so the state does not always say where
// the gateway is: the account comes from the resource identity (provider
// versions that record one), and the region from the "region" attribute
// (provider v6) or the resource identity. When they are absent they stay
// empty, and matching treats the entry as weaker (see match.compare). The
// partition is never recorded.
//
// Absent scope is normal in older state. Scope that is recorded but malformed
// or contradictory is not, and must not be read as absent: it makes the object
// unreadable, so the gateway is reported as unable to verify.
func natIdentity(id Identity, a rawAttributes, ri rawIdentity) (Identity, ProblemKind) {
	if a.ID == "" {
		return Identity{}, ProblemMissingID
	}
	if !natIDRE.MatchString(a.ID) || (ri.ID != "" && ri.ID != a.ID) {
		return Identity{}, ProblemInvalidID
	}
	if a.Region != "" && ri.Region != "" && a.Region != ri.Region {
		return Identity{}, ProblemUnreadableObject
	}
	id.ResourceID, id.Identifier = a.ID, a.ID
	id.Region = a.Region
	if id.Region == "" {
		id.Region = ri.Region
	}
	if id.Region != "" && !regionRE.MatchString(id.Region) {
		return Identity{}, ProblemUnreadableObject
	}
	if ri.AccountID != "" && !accountRE.MatchString(ri.AccountID) {
		return Identity{}, ProblemUnreadableObject
	}
	id.AccountID = ri.AccountID
	id.IdentityAccountID, id.IdentityRegion, id.IdentityIdentifier = ri.AccountID, ri.Region, ri.ID
	return id, ""
}

// lambdaIdentity reads an aws_lambda_function object. Its identity is the
// unqualified function ARN, which gives the partition, account and region.
// The name comes from the ARN too. Environment variables are in the same
// object, but rawAttributes does not declare them, so they are never decoded.
func lambdaIdentity(id Identity, a rawAttributes) (Identity, ProblemKind) {
	if a.ARN == "" {
		return Identity{}, ProblemMissingARN
	}
	arn, ok := ParseLambdaARN(a.ARN)
	if !ok {
		return Identity{}, ProblemInvalidARN
	}
	id.ARN, id.Identifier = a.ARN, arn.Name
	id.Partition, id.Region, id.AccountID = arn.Partition, arn.Region, arn.AccountID
	return id, ""
}

// bucketIdentity reads an aws_s3_bucket object. A bucket ARN
// (arn:<partition>:s3:::<name>) has no account or region, so those come
// from the state where it records them: the "region" attribute, or the
// resource identity (account_id, region) of newer provider versions. When
// they are absent they stay empty and matching treats the entry as weaker.
func bucketIdentity(id Identity, a rawAttributes, ri rawIdentity) (Identity, ProblemKind) {
	if a.ARN == "" {
		return Identity{}, ProblemMissingARN
	}
	arn, ok := ParseS3BucketARN(a.ARN)
	if !ok {
		return Identity{}, ProblemInvalidARN
	}
	name := a.Bucket
	if name == "" {
		name = a.ID
	}
	if name != "" && name != arn.Bucket {
		return Identity{}, ProblemInvalidARN // the ARN and the bucket name disagree
	}
	id.ARN, id.Identifier, id.Partition = a.ARN, arn.Bucket, arn.Partition
	id.Region = a.Region
	if id.Region == "" {
		id.Region = ri.Region
	}
	if ri.AccountID != "" && accountRE.MatchString(ri.AccountID) {
		id.AccountID = ri.AccountID
	}
	if id.Region != "" && !regionRE.MatchString(id.Region) {
		return Identity{}, ProblemUnreadableObject
	}
	id.IdentityAccountID, id.IdentityRegion, id.IdentityIdentifier = ri.AccountID, ri.Region, ri.Bucket
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
