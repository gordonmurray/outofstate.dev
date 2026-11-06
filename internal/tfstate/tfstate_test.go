package tfstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secretMarker = "OOS-SECRET-MARKER"

func load(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parse(t *testing.T, name string) *State {
	t.Helper()
	st, err := Parse(load(t, name))
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return st
}

func byAddress(st *State) map[string]Identity {
	m := map[string]Identity{}
	for _, id := range st.Identities {
		key := id.Address
		if id.DeposedKey != "" {
			key += " deposed " + id.DeposedKey
		}
		m[key] = id
	}
	return m
}

func TestProviderShapes(t *testing.T) {
	tests := []struct {
		file           string
		wantResourceID string
		wantIdentifier string
		wantSchema     int
	}{
		{"provider_v4.json", "db-V4ORDERS", "orders-db", 1},
		{"provider_v5.json", "db-V5ORDERS", "orders-db", 2},
		{"provider_v5_no_resource_id.json", "db-V5ORDERS", "orders-db", 2},
		{"provider_v6_identity.json", "db-V6ORDERS", "orders-db", 2},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			st := parse(t, tt.file)
			if st.Partial() || len(st.Identities) != 1 {
				t.Fatalf("identities=%d problems=%v", len(st.Identities), st.Problems)
			}
			id := st.Identities[0]
			if id.Address != "aws_db_instance.orders" {
				t.Errorf("Address = %q", id.Address)
			}
			if id.ResourceID != tt.wantResourceID || id.Identifier != tt.wantIdentifier || id.SchemaVersion != tt.wantSchema {
				t.Errorf("got resource_id=%q identifier=%q schema=%d", id.ResourceID, id.Identifier, id.SchemaVersion)
			}
			if id.Partition != "aws" || id.Region != "eu-west-1" || id.AccountID != "123456789012" {
				t.Errorf("scope = %s/%s/%s", id.Partition, id.Region, id.AccountID)
			}
			if st.Lineage == "" || st.Serial != 7 {
				t.Errorf("lineage=%q serial=%d", st.Lineage, st.Serial)
			}
		})
	}
}

func TestV6IdentityIsRead(t *testing.T) {
	id := parse(t, "provider_v6_identity.json").Identities[0]
	if id.IdentityAccountID != "123456789012" || id.IdentityRegion != "eu-west-1" || id.IdentityIdentifier != "orders-db" {
		t.Errorf("identity = %q %q %q", id.IdentityAccountID, id.IdentityRegion, id.IdentityIdentifier)
	}
}

func TestDBPrefixIsNotEvidence(t *testing.T) {
	// v4 shape (schema 1), identifier "db-reports", no resource_id attribute:
	// "id" is the identifier and must not be taken as a resource ID.
	id := parse(t, "provider_v4_db_prefix_identifier.json").Identities[0]
	if id.ResourceID != "" {
		t.Errorf("ResourceID = %q, want empty", id.ResourceID)
	}
	if id.Identifier != "db-reports" {
		t.Errorf("Identifier = %q", id.Identifier)
	}
}

func TestAddresses(t *testing.T) {
	st := parse(t, "addresses.json")
	got := byAddress(st)
	want := []string{
		`module.db["eu"].aws_db_instance.main[0]`,
		`module.db["eu"].aws_db_instance.main[1]`,
		`module.outer.module.inner.aws_db_instance.byname["we\"ird\\key"]`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d identities: %v", len(got), keys(got))
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing %s; have %v", w, keys(got))
		}
	}
}

func TestLifecycle(t *testing.T) {
	got := byAddress(parse(t, "lifecycle.json"))
	if got["aws_db_instance.tainted"].Lifecycle != LifecycleTainted {
		t.Errorf("tainted: %+v", got["aws_db_instance.tainted"].Lifecycle)
	}
	if got["aws_db_instance.cbd"].Lifecycle != LifecycleCurrent || got["aws_db_instance.cbd"].ResourceID != "db-NEW" {
		t.Errorf("current: %+v", got["aws_db_instance.cbd"])
	}
	dep := got["aws_db_instance.cbd deposed 00000001"]
	if dep.Lifecycle != LifecycleDeposed || dep.ResourceID != "db-OLD" {
		t.Errorf("deposed: %+v", dep)
	}
}

func TestAttributesFlat(t *testing.T) {
	st := parse(t, "attributes_flat.json")
	if len(st.Identities) != 1 || st.Identities[0].ResourceID != "db-FLAT" {
		t.Fatalf("identities: %+v", st.Identities)
	}
	if !st.Partial() || len(st.Problems) != 1 || st.Problems[0].Address != "aws_db_instance.flatbad" {
		t.Errorf("problems: %+v", st.Problems)
	}
}

func TestBadARNsMakeSourcePartialButKeepGoodIdentities(t *testing.T) {
	st := parse(t, "bad_arns.json")
	if len(st.Identities) != 1 || st.Identities[0].Identifier != "good-db" {
		t.Fatalf("identities: %+v", st.Identities)
	}
	want := map[string]ProblemKind{
		"aws_db_instance.noarn":   ProblemMissingARN,
		"aws_db_instance.cluster": ProblemInvalidARN,
		"aws_db_instance.short":   ProblemInvalidARN,
	}
	if len(st.Problems) != len(want) {
		t.Fatalf("problems: %+v", st.Problems)
	}
	for _, p := range st.Problems {
		if want[p.Address] != p.Kind {
			t.Errorf("problem %s = %s, want %s", p.Address, p.Kind, want[p.Address])
		}
	}
}

func TestEmptyStateIsOK(t *testing.T) {
	st := parse(t, "empty.json")
	if st.Partial() || len(st.Identities) != 0 {
		t.Errorf("%+v", st)
	}
}

func TestSourceErrors(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want Category
	}{
		{"empty output", []byte(""), CategoryNoSnapshot},
		{"whitespace output", []byte(" \n"), CategoryNoSnapshot},
		{"malformed", load(t, "malformed.json"), CategoryInvalidJSON},
		{"not an object", []byte(`["` + secretMarker + `"]`), CategoryInvalidJSON},
		{"version 3", load(t, "version3.json"), CategoryStateVersion},
		{"no version", []byte(`{"resources":[]}`), CategoryStateVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.data)
			var e *Error
			if !errors.As(err, &e) || e.Category != tt.want {
				t.Fatalf("err = %v, want category %s", err, tt.want)
			}
			if strings.Contains(err.Error(), secretMarker) {
				t.Errorf("error message leaks state content: %q", err.Error())
			}
		})
	}
}

func TestBadAttributeTypesAreProblemsNotLeaks(t *testing.T) {
	data := []byte(`{"version":4,"lineage":"x","serial":1,"resources":[{"mode":"managed","type":"aws_db_instance","name":"weird",
		"instances":[{"schema_version":2,"attributes":{"arn":123,"password":"` + secretMarker + `"}}]}]}`)
	st, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Problems) != 1 || st.Problems[0].Kind != ProblemUnreadableObject {
		t.Errorf("problems: %+v", st.Problems)
	}
}

// Every parsed fixture, printed in full, must not contain any secret marker.
func TestNoSecretsInParsedOutput(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		st, err := Parse(load(t, filepath.Base(f)))
		dump := fmt.Sprintf("%+v %#v %v", st, st, err)
		if strings.Contains(dump, secretMarker) {
			t.Errorf("%s: secret marker in parsed output", f)
		}
	}
}

func TestParseARN(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{"arn:aws:rds:eu-west-1:123456789012:db:orders", true},
		{"arn:aws-us-gov:rds:us-gov-west-1:123456789012:db:orders", true},
		{"arn:aws:rds:eu-west-1:123456789012:cluster:orders", false},
		{"arn:aws:rds:eu-west-1:123456789012:db:", false},
		{"arn:aws:rds::123456789012:db:orders", false},
		{"arn::rds:eu-west-1:123456789012:db:orders", false},
		{"arn:aws:rds:eu-west-1:12345678901x:db:orders", false},
		{"arn:aws:ec2:eu-west-1:123456789012:db:orders", false},
		{"orders", false},
		{"", false},
	}
	for _, tt := range tests {
		if _, ok := ParseARN(tt.in); ok != tt.ok {
			t.Errorf("ParseARN(%q) ok = %v, want %v", tt.in, ok, tt.ok)
		}
	}
}

func keys(m map[string]Identity) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// one builds a state with a single aws_db_instance resource "x" whose single
// instance object is the given JSON.
func one(instance string) []byte {
	return []byte(`{"version":4,"lineage":"l","serial":1,"resources":[{"mode":"managed","type":"aws_db_instance","name":"x","instances":[` + instance + `]}]}`)
}

const goodARN = `"arn":"arn:aws:rds:eu-west-1:123456789012:db:orders"`

func TestNormalisationEdgeCases(t *testing.T) {
	tests := []struct {
		name           string
		instance       string
		wantResourceID string
		wantIdentifier string
	}{
		{"schema 0 id is identifier", `{"schema_version":0,"attributes":{` + goodARN + `,"id":"orders"}}`, "", "orders"},
		{"schema 2 missing identifier", `{"schema_version":2,"attributes":{` + goodARN + `,"id":"db-ABC"}}`, "db-ABC", ""},
		{"schema 2 id equals identifier", `{"schema_version":2,"attributes":{` + goodARN + `,"id":"orders","identifier":"orders"}}`, "", "orders"},
		{"explicit resource_id wins over id", `{"schema_version":2,"attributes":{` + goodARN + `,"id":"db-FROMID","resource_id":"db-EXPLICIT","identifier":"orders"}}`, "db-EXPLICIT", "orders"},
		{"unknown schema 3 ignores id", `{"schema_version":3,"attributes":{` + goodARN + `,"id":"db-ABC"}}`, "", ""},
		{"unknown schema 3 keeps explicit attributes", `{"schema_version":3,"attributes":{` + goodARN + `,"id":"zzz","resource_id":"db-R","identifier":"orders"}}`, "db-R", "orders"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := Parse(one(tt.instance))
			if err != nil || len(st.Identities) != 1 {
				t.Fatalf("err=%v state=%+v", err, st)
			}
			id := st.Identities[0]
			if id.ResourceID != tt.wantResourceID || id.Identifier != tt.wantIdentifier {
				t.Errorf("resource_id=%q identifier=%q", id.ResourceID, id.Identifier)
			}
		})
	}
}

func TestObjectProblems(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		want     ProblemKind
	}{
		{"null attributes", `{"schema_version":2,"attributes":null}`, ProblemUnreadableObject},
		{"no attributes at all", `{"schema_version":2}`, ProblemUnreadableObject},
		{"bool index key", `{"index_key":true,"schema_version":2,"attributes":{` + goodARN + `}}`, ProblemUnreadableObject},
		{"fractional index key", `{"index_key":1.5,"schema_version":2,"attributes":{` + goodARN + `}}`, ProblemUnreadableObject},
		{"schema_version is a string", `{"schema_version":"2","attributes":{` + goodARN + `}}`, ProblemUnreadableObject},
		{"unknown status", `{"status":"planned","schema_version":2,"attributes":{` + goodARN + `}}`, ProblemUnknownStatus},
		{"ARN with extra segment", `{"schema_version":2,"attributes":{"arn":"arn:aws:rds:eu-west-1:123456789012:db:orders:extra"}}`, ProblemInvalidARN},
		{"ARN with whitespace", `{"schema_version":2,"attributes":{"arn":"arn:aws:rds:eu-west-1:123456789012:db:orders "}}`, ProblemInvalidARN},
		{"ARN with control char", `{"schema_version":2,"attributes":{"arn":"arn:aws:rds:eu-west-1:123456789012:db:ord\u0007ers"}}`, ProblemInvalidARN},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := Parse(one(tt.instance))
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Identities) != 0 || len(st.Problems) != 1 || st.Problems[0].Kind != tt.want {
				t.Errorf("identities=%d problems=%+v", len(st.Identities), st.Problems)
			}
		})
	}
}

func TestUnreadableResourceMakesSourcePartial(t *testing.T) {
	data := []byte(`{"version":4,"lineage":"l","serial":1,"resources":[
		{"mode":7,"type":"aws_db_instance","name":"x","instances":[]},
		{"mode":"managed","type":"aws_db_instance","name":"ok","instances":[{"schema_version":2,"attributes":{` + goodARN + `,"resource_id":"db-OK"}}]}]}`)
	st, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Identities) != 1 || len(st.Problems) != 1 || st.Problems[0].Kind != ProblemUnreadableResource {
		t.Errorf("identities=%d problems=%+v", len(st.Identities), st.Problems)
	}
}

func TestUnreadableIgnoredResourceDoesNotBreakSnapshot(t *testing.T) {
	// A malformed instance on an unrelated resource type is not decoded at all.
	data := []byte(`{"version":4,"lineage":"l","serial":1,"resources":[
		{"mode":"managed","type":"aws_s3_bucket","name":"b","instances":[{"schema_version":"weird","status":42}]},
		{"mode":"managed","type":"aws_db_instance","name":"ok","instances":[{"schema_version":2,"attributes":{` + goodARN + `,"resource_id":"db-OK"}}]}]}`)
	st, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if st.Partial() || len(st.Identities) != 1 {
		t.Errorf("identities=%d problems=%+v", len(st.Identities), st.Problems)
	}
}

func TestIdentitySchemaVersion(t *testing.T) {
	ident := `"identity":{"account_id":"123456789012","region":"eu-west-1","identifier":"orders"}`
	st, _ := Parse(one(`{"schema_version":2,"identity_schema_version":0,` + ident + `,"attributes":{` + goodARN + `}}`))
	if st.Identities[0].IdentityIdentifier != "orders" {
		t.Errorf("version 0 identity not read: %+v", st.Identities[0])
	}
	st, _ = Parse(one(`{"schema_version":2,"identity_schema_version":1,` + ident + `,"attributes":{` + goodARN + `}}`))
	if st.Identities[0].IdentityIdentifier != "" {
		t.Errorf("unknown identity version should be ignored: %+v", st.Identities[0])
	}
}

func TestQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{``, `""`},
		{`plain`, `"plain"`},
		{`a"b\c`, `"a\"b\\c"`},
		{"tab\there\nnl\rcr", `"tab\there\nnl\rcr"`},
		{`${var}`, `"$${var}"`},
		{`%{if}`, `"%%{if}"`},
		{`$x %y $`, `"$x %y $"`},
		{"bell\a", `"bell\u0007"`},
		{"del\u007f", `"del\u007f"`},
		{"zw\u200b", "\"zw\\u200b\""},
		{"tag\U000E0001", `"tag\U000e0001"`},
		{"café 東京", `"café 東京"`},
	}
	for _, tt := range tests {
		if got := quote(tt.in); got != tt.want {
			t.Errorf("quote(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestValidIdentifier(t *testing.T) {
	for in, want := range map[string]bool{
		"orders": true, "Orders-DB-1": true, "a": true, strings.Repeat("a", 63): true,
		strings.Repeat("a", 64): false, "1orders": false, "orders-": false, "or--ders": false,
		"or_ders": false, "": false, "or ders": false,
	} {
		if got := ValidIdentifier(in); got != want {
			t.Errorf("ValidIdentifier(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResourcesWithMissingFieldsArePartial(t *testing.T) {
	okRes := `{"mode":"managed","type":"aws_db_instance","name":"ok","instances":[{"schema_version":2,"attributes":{` + goodARN + `,"resource_id":"db-OK"}}]}`
	tests := map[string]string{
		"null resource entry": `null`,
		"null mode":           `{"mode":null,"type":"aws_db_instance","name":"x","instances":[]}`,
		"missing mode":        `{"type":"aws_db_instance","name":"x","instances":[]}`,
		"unknown mode":        `{"mode":"other","type":"aws_db_instance","name":"x","instances":[]}`,
		"null type":           `{"mode":"managed","type":null,"name":"x","instances":[]}`,
		"missing type":        `{"mode":"managed","name":"x","instances":[]}`,
		"missing name":        `{"mode":"managed","type":"aws_db_instance","instances":[]}`,
	}
	for name, bad := range tests {
		t.Run(name, func(t *testing.T) {
			st, err := Parse([]byte(`{"version":4,"lineage":"l","serial":1,"resources":[` + bad + `,` + okRes + `]}`))
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Identities) != 1 || len(st.Problems) != 1 || st.Problems[0].Kind != ProblemUnreadableResource {
				t.Errorf("identities=%d problems=%+v", len(st.Identities), st.Problems)
			}
		})
	}
}

func TestNullInstanceEntryIsAProblem(t *testing.T) {
	st, err := Parse(one(`null`))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Problems) != 1 || st.Problems[0].Kind != ProblemUnreadableObject {
		t.Errorf("problems=%+v", st.Problems)
	}
}
