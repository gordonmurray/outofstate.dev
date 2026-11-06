package match

import (
	"slices"
	"testing"

	"github.com/gordonmurray/outofstate.dev/internal/discover"
	"github.com/gordonmurray/outofstate.dev/internal/tfstate"
)

const (
	acctA = "111111111111"
	acctB = "222222222222"
	euw1  = "eu-west-1"
	use1  = "us-east-1"
)

func arn(acct, region, name string) string {
	return "arn:aws:rds:" + region + ":" + acct + ":db:" + name
}

func inst(name, rid string) discover.Instance {
	return instIn(acctA, euw1, name, rid)
}

func instIn(acct, region, name, rid string) discover.Instance {
	return discover.Instance{
		Partition: "aws", AccountID: acct, Region: region,
		ARN: arn(acct, region, name), Identifier: name, ResourceID: rid,
	}
}

func ident(addr, name, rid string) tfstate.Identity {
	return identIn(acctA, euw1, addr, name, rid)
}

func identIn(acct, region, addr, name, rid string) tfstate.Identity {
	return tfstate.Identity{
		Type:    "aws_db_instance",
		Address: addr, Lifecycle: tfstate.LifecycleCurrent, SchemaVersion: 2,
		ARN: arn(acct, region, name), Identifier: name, ResourceID: rid,
		Partition: "aws", AccountID: acct, Region: region,
	}
}

func src(name, lineage string, serial int64, ids ...tfstate.Identity) Source {
	return Source{Name: name, Workspace: "default", Status: SourceOK,
		State: &tfstate.State{Lineage: lineage, Serial: serial, Identities: ids}}
}

func failed(name string) Source {
	return Source{Name: name, Workspace: "default", Status: SourceFailed}
}

func partial(s Source) Source { s.Status = SourcePartial; return s }

func hasWarning(ws []Warning, k WarningKind) bool {
	for _, w := range ws {
		if w.Kind == k {
			return true
		}
	}
	return false
}

func TestRun(t *testing.T) {
	tests := []struct {
		name      string
		instances []discover.Instance
		sources   []Source
		// expectations for the first finding
		status       Status
		keys         []Key
		renamed      bool
		warnings     []WarningKind
		scanWarnings []WarningKind
		complete     bool
	}{
		{
			name:      "match on resource id",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources:   []Source{src("a", "L1", 1, ident("aws_db_instance.orders", "orders", "db-1"))},
			status:    StatusTracked, keys: []Key{KeyResourceID}, complete: true,
		},
		{
			name:      "recreated database: same ARN, new resource id",
			instances: []discover.Instance{inst("orders", "db-NEW")},
			sources:   []Source{src("a", "L1", 1, ident("aws_db_instance.orders", "orders", "db-OLD"))},
			status:    StatusNoMatch, warnings: []WarningKind{WarnStaleStateEntry}, complete: true,
		},
		{
			name:      "renamed since state was written",
			instances: []discover.Instance{inst("orders-v2", "db-1")},
			sources:   []Source{src("a", "L1", 1, ident("aws_db_instance.orders", "orders", "db-1"))},
			status:    StatusTracked, keys: []Key{KeyResourceID}, renamed: true, complete: true,
		},
		{
			name:      "same identifier in another account",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources:   []Source{src("a", "L1", 1, identIn(acctB, euw1, "aws_db_instance.orders", "orders", ""))},
			status:    StatusNoMatch, complete: true,
		},
		{
			name:      "same identifier and resource id in another region",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources:   []Source{src("a", "L1", 1, identIn(acctA, use1, "aws_db_instance.orders", "orders", "db-1"))},
			status:    StatusNoMatch, complete: true,
		},
		{
			name:      "state entry without resource id matches on ARN (weak)",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources:   []Source{src("a", "L1", 1, ident("aws_db_instance.orders", "orders", ""))},
			status:    StatusTracked, keys: []Key{KeyARN}, complete: true,
		},
		{
			name:      "identifier starting with db- is matched by ARN, not mistaken for a resource id",
			instances: []discover.Instance{inst("db-reports", "db-REAL")},
			sources:   []Source{src("a", "L1", 1, ident("aws_db_instance.r", "db-reports", ""))},
			status:    StatusTracked, keys: []Key{KeyARN}, complete: true,
		},
		{
			name:      "deposed object matches the old instance",
			instances: []discover.Instance{inst("old", "db-OLD")},
			sources: []Source{src("a", "L1", 1,
				ident("aws_db_instance.cbd", "new", "db-NEW"),
				func() tfstate.Identity {
					i := ident("aws_db_instance.cbd", "old", "db-OLD")
					i.Lifecycle, i.DeposedKey = tfstate.LifecycleDeposed, "00000001"
					return i
				}())},
			status: StatusTracked, keys: []Key{KeyResourceID}, complete: true,
		},
		{
			name:      "two different states manage it",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources: []Source{
				src("a", "L1", 1, ident("aws_db_instance.orders", "orders", "db-1")),
				src("b", "L2", 1, ident("aws_db_instance.db", "orders", "db-1")),
			},
			status: StatusTracked, keys: []Key{KeyResourceID, KeyResourceID},
			warnings: []WarningKind{WarnMultipleOwners}, complete: true,
		},
		{
			name:      "mixed keys across states",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources: []Source{
				src("a", "L1", 1, ident("aws_db_instance.orders", "orders", "db-1")),
				src("b", "L2", 1, ident("aws_db_instance.db", "orders", "")),
			},
			status: StatusTracked, keys: []Key{KeyResourceID, KeyARN},
			warnings: []WarningKind{WarnMultipleOwners}, complete: true,
		},
		{
			name:      "same snapshot supplied twice counts once",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources: []Source{
				src("./infra", "L1", 5, ident("aws_db_instance.orders", "orders", "db-1")),
				src("export.json", "L1", 5, ident("aws_db_instance.orders", "orders", "db-1")),
			},
			status: StatusTracked, keys: []Key{KeyResourceID},
			scanWarnings: []WarningKind{WarnDuplicateSnapshot}, complete: true,
		},
		{
			name:      "same lineage at different serials: only the newest is used",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources: []Source{
				src("./infra", "L1", 6, ident("aws_db_instance.orders", "orders", "db-1")),
				src("old-export.json", "L1", 5, ident("aws_db_instance.orders", "orders", "db-1")),
			},
			status: StatusTracked, keys: []Key{KeyResourceID},
			scanWarnings: []WarningKind{WarnDivergedCopies}, complete: true,
		},
		{
			name:      "states without lineage are never merged",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources: []Source{
				src("x.json", "", 1, ident("aws_db_instance.orders", "orders", "db-1")),
				src("y.json", "", 1, ident("aws_db_instance.orders", "orders", "db-1")),
			},
			status: StatusTracked, keys: []Key{KeyResourceID, KeyResourceID},
			warnings: []WarningKind{WarnMultipleOwners}, complete: true,
		},
		{
			name:      "two addresses in one state",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources: []Source{src("a", "L1", 1,
				ident("aws_db_instance.orders", "orders", "db-1"),
				ident("module.copy.aws_db_instance.orders", "orders", "db-1"))},
			status: StatusTracked, keys: []Key{KeyResourceID, KeyResourceID},
			warnings: []WarningKind{WarnMultipleAddresses}, complete: true,
		},
		{
			name:      "stale entry in one state, real match in another",
			instances: []discover.Instance{inst("orders", "db-NEW")},
			sources: []Source{
				src("a", "L1", 1, ident("aws_db_instance.old", "orders", "db-OLD")),
				src("b", "L2", 1, ident("aws_db_instance.orders", "orders", "db-NEW")),
			},
			status: StatusTracked, keys: []Key{KeyResourceID},
			warnings: []WarningKind{WarnStaleStateEntry}, complete: true,
		},
		{
			name:      "a failed source makes unmatched instances unverifiable",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources:   []Source{src("a", "L1", 1), failed("b")},
			status:    StatusUnverified, complete: false,
		},
		{
			name:      "a partial source makes unmatched instances unverifiable",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources:   []Source{partial(src("a", "L1", 1))},
			status:    StatusUnverified, complete: false,
		},
		{
			name:      "matched instances stay tracked when another source failed",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources:   []Source{src("a", "L1", 1, ident("aws_db_instance.orders", "orders", "db-1")), failed("b")},
			status:    StatusTracked, keys: []Key{KeyResourceID}, complete: false,
		},
		{
			name: "not covered instances are not matched",
			instances: []discover.Instance{func() discover.Instance {
				i := inst("aurora-1", "db-A")
				i.ClusterID = "aurora"
				discover.Classify(&i)
				return i
			}()},
			sources: []Source{src("a", "L1", 1, ident("aws_db_instance.x", "aurora-1", "db-A"))},
			status:  StatusNotCovered, complete: true,
		},
		{
			name:      "instance without resource id never matches a state entry that has one",
			instances: []discover.Instance{instIn(acctA, euw1, "orders", "")},
			sources:   []Source{src("a", "L1", 1, ident("aws_db_instance.orders", "orders", "db-1"))},
			status:    StatusNoMatch, complete: true,
		},
		{
			name:      "identifier fallback when neither side has an ARN match and the state entry has no resource id",
			instances: []discover.Instance{{Partition: "aws", AccountID: acctA, Region: euw1, Identifier: "orders", ResourceID: "db-1"}},
			sources:   []Source{src("a", "L1", 1, ident("aws_db_instance.orders", "orders", ""))},
			status:    StatusTracked, keys: []Key{KeyIdentifier}, complete: true,
		},
		{
			name:      "same account, region and ids in another partition",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources: []Source{src("a", "L1", 1, func() tfstate.Identity {
				i := ident("aws_db_instance.orders", "orders", "db-1")
				i.Partition = "aws-us-gov"
				return i
			}())},
			status: StatusNoMatch, complete: true,
		},
		{
			name:      "empty values are never compared",
			instances: []discover.Instance{{Partition: "aws", AccountID: acctA, Region: euw1}},
			sources: []Source{src("a", "L1", 1, tfstate.Identity{
				Address: "aws_db_instance.blank", Partition: "aws", AccountID: acctA, Region: euw1})},
			status: StatusNoMatch, complete: true,
		},
		{
			name:      "a match from a partial source still counts",
			instances: []discover.Instance{inst("orders", "db-1")},
			sources:   []Source{partial(src("a", "L1", 1, ident("aws_db_instance.orders", "orders", "db-1")))},
			status:    StatusTracked, keys: []Key{KeyResourceID}, complete: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Run(tt.instances, tt.sources)
			if res.StateComplete != tt.complete {
				t.Errorf("StateComplete = %v, want %v", res.StateComplete, tt.complete)
			}
			if len(res.Findings) != len(tt.instances) {
				t.Fatalf("findings = %d", len(res.Findings))
			}
			f := res.Findings[0]
			if f.Status != tt.status {
				t.Errorf("Status = %s, want %s", f.Status, tt.status)
			}
			var keys []Key
			renamed := false
			for _, m := range f.Matches {
				keys = append(keys, m.MatchedOn)
				renamed = renamed || m.Renamed
			}
			if !slices.Equal(keys, tt.keys) {
				t.Errorf("keys = %v, want %v", keys, tt.keys)
			}
			if renamed != tt.renamed {
				t.Errorf("renamed = %v, want %v", renamed, tt.renamed)
			}
			checkWarnings(t, "finding", f.Warnings, tt.warnings)
			checkWarnings(t, "scan", res.Warnings, tt.scanWarnings)
		})
	}
}

func TestNoInstances(t *testing.T) {
	res := Run(nil, []Source{src("a", "L1", 1)})
	if len(res.Findings) != 0 || !res.StateComplete {
		t.Errorf("%+v", res)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		engine, cluster, want string
	}{
		{"postgres", "", ""},
		{"mysql", "", ""},
		{"aurora-postgresql", "my-cluster", discover.NotCoveredClusterMember},
		{"mysql", "multi-az-cluster", discover.NotCoveredClusterMember},
		{"docdb", "docdb-cluster", discover.NotCoveredDocumentDB},
		{"neptune", "neptune-cluster", discover.NotCoveredNeptune},
	}
	for _, tt := range tests {
		i := discover.Instance{Engine: tt.engine, ClusterID: tt.cluster}
		discover.Classify(&i)
		if i.NotCovered != tt.want {
			t.Errorf("Classify(%s, %q) = %q, want %q", tt.engine, tt.cluster, i.NotCovered, tt.want)
		}
	}
}

func checkWarnings(t *testing.T, what string, got []Warning, want []WarningKind) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s warnings = %+v, want %v", what, got, want)
		return
	}
	for _, k := range want {
		if !hasWarning(got, k) {
			t.Errorf("%s warnings = %+v, missing %s", what, got, k)
		}
	}
}

func TestWarningDetails(t *testing.T) {
	res := Run([]discover.Instance{inst("orders", "db-NEW")}, []Source{
		src("a", "L1", 1,
			ident("aws_db_instance.old", "orders", "db-OLD"),
			ident("aws_db_instance.orders", "orders", "db-NEW"),
			ident("module.copy.aws_db_instance.orders", "orders", "db-NEW")),
		src("b", "L2", 1, ident("aws_db_instance.db", "orders", "db-NEW")),
	})
	f := res.Findings[0]
	byKind := map[WarningKind]Warning{}
	for _, w := range f.Warnings {
		byKind[w.Kind] = w
	}
	if w := byKind[WarnStaleStateEntry]; !slices.Equal(w.Sources, []string{"a"}) || !slices.Equal(w.Addresses, []string{"aws_db_instance.old"}) {
		t.Errorf("stale: %+v", w)
	}
	if w := byKind[WarnMultipleAddresses]; !slices.Equal(w.Sources, []string{"a"}) ||
		!slices.Equal(w.Addresses, []string{"aws_db_instance.orders", "module.copy.aws_db_instance.orders"}) {
		t.Errorf("multiple addresses: %+v", w)
	}
	if w := byKind[WarnMultipleOwners]; !slices.Equal(w.Sources, []string{"a", "b"}) {
		t.Errorf("multiple owners: %+v", w)
	}

	m := f.Matches[0]
	if m.Source != "a" || m.Workspace != "default" || m.Identity.Address != "aws_db_instance.orders" || m.MatchedOn != KeyResourceID {
		t.Errorf("match metadata: %+v", m)
	}
}

func TestScanWarningDetails(t *testing.T) {
	res := Run(nil, []Source{
		src("./infra", "L1", 6),
		src("copy.json", "L1", 6),
		src("old.json", "L1", 5),
	})
	byKind := map[WarningKind]Warning{}
	for _, w := range res.Warnings {
		byKind[w.Kind] = w
	}
	if w := byKind[WarnDuplicateSnapshot]; !slices.Equal(w.Sources, []string{"./infra", "copy.json"}) {
		t.Errorf("duplicate: %+v", w)
	}
	if w := byKind[WarnDivergedCopies]; !slices.Equal(w.Sources, []string{"./infra", "old.json"}) {
		t.Errorf("diverged: %+v", w)
	}
}

// An older snapshot of the same state must never make an instance look
// tracked after the newer snapshot dropped it, whatever the argument order.
func TestOlderSnapshotCannotHideACurrentGap(t *testing.T) {
	olderTracks := src("old-export.json", "L1", 5, ident("aws_db_instance.orders", "orders", "db-1"))
	newerDropped := src("./infra", "L1", 6)
	tests := []struct {
		name     string
		sources  []Source
		want     Status
		diverged []string
	}{
		{"older only", []Source{olderTracks}, StatusTracked, nil},
		{"newer only", []Source{newerDropped}, StatusNoMatch, nil},
		{"both, older first", []Source{olderTracks, newerDropped}, StatusNoMatch, []string{"./infra", "old-export.json"}},
		{"both, newer first", []Source{newerDropped, olderTracks}, StatusNoMatch, []string{"./infra", "old-export.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Run([]discover.Instance{inst("orders", "db-1")}, tt.sources)
			if got := res.Findings[0].Status; got != tt.want {
				t.Errorf("status = %s, want %s", got, tt.want)
			}
			var diverged []string
			for _, w := range res.Warnings {
				if w.Kind == WarnDivergedCopies {
					diverged = w.Sources
				}
			}
			if !slices.Equal(diverged, tt.diverged) {
				t.Errorf("diverged_copies sources = %v, want %v (newest first)", diverged, tt.diverged)
			}
			if !res.StateComplete {
				t.Error("an ignored older snapshot must not make the scan partial")
			}
		})
	}
}

func TestIgnoredOlderPartialSnapshotDoesNotMakeScanPartial(t *testing.T) {
	res := Run([]discover.Instance{inst("orders", "db-1")}, []Source{
		src("./infra", "L1", 6, ident("aws_db_instance.orders", "orders", "db-1")),
		partial(src("old.json", "L1", 5)),
	})
	if !res.StateComplete || res.Findings[0].Status != StatusTracked {
		t.Errorf("%+v", res)
	}
	// A partial newest snapshot does make it partial.
	res = Run([]discover.Instance{inst("orders", "db-2")}, []Source{
		partial(src("./infra", "L1", 6)),
		src("old.json", "L1", 5, ident("aws_db_instance.orders", "orders", "db-2")),
	})
	if res.StateComplete || res.Findings[0].Status != StatusUnverified {
		t.Errorf("%+v", res)
	}
}

func TestSnapshotSelectionEdgeCases(t *testing.T) {
	id := ident("aws_db_instance.orders", "orders", "db-1")
	tests := []struct {
		name     string
		sources  []Source
		status   Status
		complete bool
		warnings []WarningKind
	}{
		{"older serial supplied twice: both ignored", []Source{
			src("old1", "L1", 5, id), src("old2", "L1", 5, id), src("new", "L1", 6),
		}, StatusNoMatch, true, []WarningKind{WarnDivergedCopies}},
		{"newest twice plus an older copy, older first", []Source{
			src("old", "L1", 5, id), src("new1", "L1", 6), src("new2", "L1", 6),
		}, StatusNoMatch, true, []WarningKind{WarnDuplicateSnapshot, WarnDivergedCopies}},
		{"newest twice plus an older copy, newest first", []Source{
			src("new1", "L1", 6), src("old", "L1", 5, id), src("new2", "L1", 6),
		}, StatusNoMatch, true, []WarningKind{WarnDuplicateSnapshot, WarnDivergedCopies}},
		{"identical newest copies, partial one first: the full one is used", []Source{
			partial(src("new-partial", "L1", 6)), src("new-ok", "L1", 6),
		}, StatusNoMatch, true, []WarningKind{WarnDuplicateSnapshot}},
		{"identical newest copies, full one first", []Source{
			src("new-ok", "L1", 6), partial(src("new-partial", "L1", 6)),
		}, StatusNoMatch, true, []WarningKind{WarnDuplicateSnapshot}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Run([]discover.Instance{inst("orders", "db-1")}, tt.sources)
			if res.Findings[0].Status != tt.status || res.StateComplete != tt.complete {
				t.Errorf("status=%s complete=%v", res.Findings[0].Status, res.StateComplete)
			}
			checkWarnings(t, "scan", res.Warnings, tt.warnings)
		})
	}
}

func lbARN(name, id string) string {
	return "arn:aws:elasticloadbalancing:" + euw1 + ":" + acctA + ":loadbalancer/app/" + name + "/" + id
}

func lbInst(name, id string) discover.Instance {
	return discover.Instance{Type: discover.TypeLoadBalancer, Kind: discover.KindALB,
		Partition: "aws", AccountID: acctA, Region: euw1, ARN: lbARN(name, id), Identifier: name}
}

// lbIdent is a state entry for the load balancer with suffix 0000000000000001.
func lbIdent(addr, name string) tfstate.Identity {
	return tfstate.Identity{Type: "aws_lb", Address: addr, Lifecycle: tfstate.LifecycleCurrent,
		ARN: lbARN(name, "0000000000000001"), Identifier: name, Partition: "aws", AccountID: acctA, Region: euw1}
}

func TestLoadBalancerMatching(t *testing.T) {
	tests := []struct {
		name   string
		inst   discover.Instance
		ids    []tfstate.Identity
		status Status
		weak   bool
	}{
		{"same ARN is a strong match", lbInst("web", "0000000000000001"),
			[]tfstate.Identity{lbIdent("aws_lb.web", "web")}, StatusTracked, false},
		{"consecutive hyphens in the name", lbInst("web--prod", "0000000000000001"),
			[]tfstate.Identity{lbIdent("aws_lb.web", "web--prod")}, StatusTracked, false},
		{"recreated with the same name: new ARN, no match", lbInst("web", "0000000000000002"),
			[]tfstate.Identity{lbIdent("aws_lb.web", "web")}, StatusNoMatch, false},
		{"same name in state is not enough", lbInst("web", "0000000000000002"),
			[]tfstate.Identity{{Type: "aws_lb", Address: "aws_lb.web", Identifier: "web", Partition: "aws", AccountID: acctA, Region: euw1}}, StatusNoMatch, false},
		{"an RDS entry never matches a load balancer", lbInst("orders", "0000000000000001"),
			[]tfstate.Identity{ident("aws_db_instance.orders", "orders", "")}, StatusNoMatch, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Run([]discover.Instance{tt.inst}, []Source{src("a", "L1", 1, tt.ids...)})
			f := res.Findings[0]
			if f.Status != tt.status {
				t.Fatalf("status = %s, want %s", f.Status, tt.status)
			}
			if tt.status == StatusTracked && (f.Matches[0].MatchedOn != KeyARN || f.Matches[0].Weak != tt.weak) {
				t.Errorf("match = %+v", f.Matches[0])
			}
		})
	}
}

func TestRDSNeverMatchesLoadBalancerEntries(t *testing.T) {
	// A load balancer named like a database must not match by identifier.
	res := Run([]discover.Instance{inst("orders", "db-1")}, []Source{src("a", "L1", 1, lbIdent("aws_lb.orders", "orders"))})
	if res.Findings[0].Status != StatusNoMatch {
		t.Errorf("status = %s", res.Findings[0].Status)
	}
}

func TestRDSWeakMatches(t *testing.T) {
	for _, tt := range []struct {
		name string
		inst discover.Instance
		id   tfstate.Identity
		key  Key
		weak bool
	}{
		{"resource id: strong", inst("orders", "db-1"), ident("aws_db_instance.o", "orders", "db-1"), KeyResourceID, false},
		{"ARN without a resource id in state: weak", inst("orders", "db-1"), ident("aws_db_instance.o", "orders", ""), KeyARN, true},
		{"identifier only: weak", discover.Instance{Partition: "aws", AccountID: acctA, Region: euw1, Identifier: "orders", ResourceID: "db-1"}, ident("aws_db_instance.o", "orders", ""), KeyIdentifier, true},
	} {
		res := Run([]discover.Instance{tt.inst}, []Source{src("a", "L1", 1, tt.id)})
		m := res.Findings[0].Matches
		if len(m) != 1 || m[0].MatchedOn != tt.key || m[0].Weak != tt.weak {
			t.Errorf("%s: %+v", tt.name, m)
		}
	}
}

func bucketInst(name, region string) discover.Instance {
	return discover.Instance{Type: discover.TypeS3Bucket, Kind: discover.KindS3, Partition: "aws", AccountID: acctA,
		Region: region, ARN: "arn:aws:s3:::" + name, Identifier: name}
}

func bucketIdent(addr, name, region, account string) tfstate.Identity {
	return tfstate.Identity{Type: "aws_s3_bucket", Address: addr, Lifecycle: tfstate.LifecycleCurrent,
		ARN: "arn:aws:s3:::" + name, Identifier: name, Partition: "aws", Region: region, AccountID: account}
}

func TestS3Matching(t *testing.T) {
	tests := []struct {
		name     string
		inst     discover.Instance
		id       tfstate.Identity
		status   Status
		weak     bool
		warnings []WarningKind
	}{
		{"name and region", bucketInst("logs", euw1), bucketIdent("aws_s3_bucket.logs", "logs", euw1, ""), StatusTracked, false, nil},
		{"name, region and account", bucketInst("logs", euw1), bucketIdent("aws_s3_bucket.logs", "logs", euw1, acctA), StatusTracked, false, nil},
		{"no region in state: weaker", bucketInst("logs", euw1), bucketIdent("aws_s3_bucket.logs", "logs", "", ""), StatusTracked, true, nil},
		{"state says another region: stale", bucketInst("logs", use1), bucketIdent("aws_s3_bucket.logs", "logs", euw1, ""), StatusNoMatch, false, []WarningKind{WarnStaleStateEntry}},
		{"state from another account", bucketInst("logs", euw1), bucketIdent("aws_s3_bucket.logs", "logs", euw1, acctB), StatusNoMatch, false, nil},
		{"different bucket", bucketInst("logs", euw1), bucketIdent("aws_s3_bucket.other", "other", euw1, ""), StatusNoMatch, false, nil},
		{"same bucket, other name in state", bucketInst("exports", euw1), bucketIdent("aws_s3_bucket.logs", "logs", euw1, ""), StatusNoMatch, false, nil},
		{"legacy bucket name", bucketInst("Legacy_Bucket.2017", use1), bucketIdent("aws_s3_bucket.legacy", "Legacy_Bucket.2017", use1, ""), StatusTracked, false, nil},
		{"other partition", bucketInst("logs", euw1), func() tfstate.Identity {
			i := bucketIdent("aws_s3_bucket.logs", "logs", euw1, "")
			i.Partition, i.ARN = "aws-cn", "arn:aws-cn:s3:::logs"
			return i
		}(), StatusNoMatch, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Run([]discover.Instance{tt.inst}, []Source{src("a", "L1", 1, tt.id)})
			f := res.Findings[0]
			if f.Status != tt.status {
				t.Fatalf("status = %s, want %s", f.Status, tt.status)
			}
			if tt.status == StatusTracked && (f.Matches[0].MatchedOn != KeyARN || f.Matches[0].Weak != tt.weak) {
				t.Errorf("match = %+v", f.Matches[0])
			}
			checkWarnings(t, "finding", f.Warnings, tt.warnings)
		})
	}
}
