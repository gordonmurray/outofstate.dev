// Package match decides, for each discovered RDS instance, whether any
// supplied Terraform state tracks it. It is a pure function: no I/O.
package match

import (
	"slices"
	"strconv"
	"strings"

	"github.com/gordonmurray/outofstate.dev/internal/discover"
	"github.com/gordonmurray/outofstate.dev/internal/tfstate"
)

// SourceStatus is how completely a Terraform source was read.
type SourceStatus string

// Source statuses.
const (
	SourceOK      SourceStatus = "ok"
	SourcePartial SourceStatus = "partial"
	SourceFailed  SourceStatus = "failed"
)

// Source is one supplied Terraform state: a working directory or a file.
type Source struct {
	Name      string // path shown in the report
	Workspace string // "unknown" for state files
	Status    SourceStatus
	State     *tfstate.State // nil when Status is SourceFailed
}

// Status of one instance.
type Status string

// Instance statuses.
const (
	StatusTracked    Status = "tracked"
	StatusNoMatch    Status = "no_match"
	StatusUnverified Status = "unable_to_verify"
	StatusNotCovered Status = "not_covered"
)

// Key is the identity key a match was made on.
type Key string

// Match keys, strongest first.
const (
	KeyResourceID Key = "resource_id"
	KeyARN        Key = "arn"        // weaker: state entry has no resource ID
	KeyIdentifier Key = "identifier" // weaker: state entry has no resource ID
)

// Match links an instance to one state entry.
type Match struct {
	Source    string
	Workspace string
	Identity  tfstate.Identity
	MatchedOn Key
	// Weak is set when the match did not use the resource type's immutable
	// identity (for RDS: an ARN or name match because the state entry had no
	// resource ID).
	Weak bool
	// Renamed is set when the resource IDs match but the ARNs differ: the
	// instance was renamed after the state was last written.
	Renamed bool
}

// WarningKind classifies a warning.
type WarningKind string

// Warning kinds.
const (
	// Per instance.
	WarnMultipleOwners    WarningKind = "multiple_owners"    // matched in states with different lineage
	WarnMultipleAddresses WarningKind = "multiple_addresses" // matched at more than one address in one state
	WarnStaleStateEntry   WarningKind = "stale_state_entry"  // same ARN, different resource ID
	// Per scan.
	WarnDuplicateSnapshot WarningKind = "duplicate_snapshot" // same lineage and serial supplied twice
	WarnDivergedCopies    WarningKind = "diverged_copies"    // same lineage, different serial: first source (newest) used, the rest ignored
)

// Warning carries the sources and addresses involved, never state values.
type Warning struct {
	Kind      WarningKind
	Sources   []string
	Addresses []string
}

// Finding is the result for one instance.
type Finding struct {
	Instance discover.Instance
	Status   Status
	Matches  []Match
	Warnings []Warning
}

// Result of matching.
type Result struct {
	Findings []Finding
	Warnings []Warning
	// StateComplete is true when no source failed and every snapshot used
	// for matching parsed fully. Ignored older snapshots do not count.
	StateComplete bool
}

// keptSource is a source whose identities take part in matching, after
// duplicate snapshots are removed.
type keptSource struct {
	Source
	owner string // lineage, or a per-source key when lineage is empty
}

// Run matches instances against sources.
func Run(instances []discover.Instance, sources []Source) Result {
	kept, warnings := dedupe(sources)
	res := Result{StateComplete: true, Warnings: warnings}

	// Completeness counts every failed source (its lineage is unknown, so it
	// cannot be ruled out) and every source used for matching. An older
	// snapshot that was ignored does not count.
	for _, s := range sources {
		if s.State == nil || s.Status == SourceFailed {
			res.StateComplete = false
		}
	}
	for _, k := range kept {
		if k.Status != SourceOK {
			res.StateComplete = false
		}
	}

	for _, inst := range instances {
		res.Findings = append(res.Findings, matchInstance(inst, kept, res.StateComplete))
	}
	return res
}

// dedupe decides which snapshots take part in matching:
//
//   - for each lineage (one Terraform state over time) only the newest
//     snapshot (highest serial) is used. An older export must never make an
//     instance look tracked after the current state dropped it. The skipped
//     snapshots are reported in a diverged_copies warning, newest first;
//   - the same lineage and serial supplied twice is counted once
//     (duplicate_snapshot), keeping the copy that parsed best. Copies of an
//     older serial are only listed in diverged_copies;
//   - states without a lineage are never merged with anything.
func dedupe(sources []Source) ([]keptSource, []Warning) {
	newest := map[string]int64{} // lineage -> highest serial
	for _, s := range sources {
		if s.State == nil || s.State.Lineage == "" {
			continue
		}
		if cur, ok := newest[s.State.Lineage]; !ok || s.State.Serial > cur {
			newest[s.State.Lineage] = s.State.Serial
		}
	}

	var (
		kept       []keptSource
		warnings   []Warning
		usedName   = map[string]string{}   // lineage -> source used
		older      = map[string][]string{} // lineage -> ignored older sources
		lineageOrd []string
	)
	for i, s := range sources {
		if s.State == nil {
			continue
		}
		lin := s.State.Lineage
		if lin == "" {
			kept = append(kept, keptSource{Source: s, owner: "\x00source-" + strconv.Itoa(i)})
			continue
		}
		if _, seen := usedName[lin]; !seen && older[lin] == nil {
			lineageOrd = append(lineageOrd, lin)
		}
		switch {
		case s.State.Serial < newest[lin]:
			older[lin] = append(older[lin], s.Name)
		case usedName[lin] != "":
			// The same snapshot again. Keep the copy that parsed best, so the
			// result does not depend on argument order.
			prev := usedName[lin]
			for k := range kept {
				if kept[k].owner == lin && rank(s.Status) < rank(kept[k].Status) {
					kept[k].Source = s
					usedName[lin] = s.Name
				}
			}
			warnings = append(warnings, Warning{Kind: WarnDuplicateSnapshot, Sources: []string{prev, s.Name}})
		default:
			usedName[lin] = s.Name
			kept = append(kept, keptSource{Source: s, owner: lin})
		}
	}
	for _, lin := range lineageOrd {
		if len(older[lin]) > 0 {
			warnings = append(warnings, Warning{Kind: WarnDivergedCopies, Sources: append([]string{usedName[lin]}, older[lin]...)})
		}
	}
	return kept, warnings
}

func matchInstance(inst discover.Instance, sources []keptSource, stateComplete bool) Finding {
	f := Finding{Instance: inst}
	if inst.NotCovered != "" {
		f.Status = StatusNotCovered
		return f
	}

	owners := map[string]bool{}
	var ownerSources []string
	addrsBySource := make([][]string, len(sources))
	var staleSources, staleAddrs []string

	for si, src := range sources {
		for _, id := range src.State.Identities {
			if id.Type != inst.TerraformType() ||
				id.Partition != inst.Partition || id.AccountID != inst.AccountID || id.Region != inst.Region {
				continue
			}
			key, weak, renamed, stale := compare(inst, id)
			if stale {
				staleSources = append(staleSources, src.Name)
				staleAddrs = append(staleAddrs, id.Address)
			}
			if key == "" {
				continue
			}
			f.Matches = append(f.Matches, Match{
				Source: src.Name, Workspace: src.Workspace, Identity: id, MatchedOn: key, Weak: weak, Renamed: renamed,
			})
			if !owners[src.owner] {
				owners[src.owner] = true
				ownerSources = append(ownerSources, src.Name)
			}
			if !slices.Contains(addrsBySource[si], id.Address) {
				addrsBySource[si] = append(addrsBySource[si], id.Address)
			}
		}
	}

	switch {
	case len(f.Matches) > 0:
		f.Status = StatusTracked
	case stateComplete:
		f.Status = StatusNoMatch
	default:
		f.Status = StatusUnverified
	}

	if len(owners) > 1 {
		f.Warnings = append(f.Warnings, Warning{Kind: WarnMultipleOwners, Sources: ownerSources})
	}
	for si, src := range sources {
		if addrs := addrsBySource[si]; len(addrs) > 1 {
			f.Warnings = append(f.Warnings, Warning{Kind: WarnMultipleAddresses, Sources: []string{src.Name}, Addresses: addrs})
		}
	}
	if len(staleSources) > 0 {
		f.Warnings = append(f.Warnings, Warning{Kind: WarnStaleStateEntry, Sources: staleSources, Addresses: staleAddrs})
	}
	return f
}

// compare decides whether a state entry tracks a resource of the same type.
//
// Load balancers: the ARN is the identity (it ends in a generated suffix, so
// a recreated load balancer has a new ARN). An equal ARN is a strong match.
//
// RDS DB instances: when both sides have a resource ID, only the
// resource ID decides; a different resource ID with the same ARN is a stale
// state entry (the database was recreated) and never falls back to ARN or
// name. Fallback is allowed only when the state entry has no resource ID:
// an exact ARN, then the identifier within the same scope, is a weaker match.
func compare(inst discover.Instance, id tfstate.Identity) (key Key, weak, renamed, stale bool) {
	if inst.TerraformType() == discover.TypeLoadBalancer {
		if inst.ARN != "" && inst.ARN == id.ARN {
			return KeyARN, false, false, false
		}
		return "", false, false, false
	}
	if id.ResourceID != "" {
		// The state recorded the immutable ID, so only that ID can confirm
		// ownership. If AWS did not return one, the match cannot be verified.
		if inst.ResourceID == "" {
			return "", false, false, false
		}
		if inst.ResourceID == id.ResourceID {
			return KeyResourceID, false, inst.ARN != id.ARN, false
		}
		return "", false, false, inst.ARN != "" && inst.ARN == id.ARN
	}
	if inst.ARN != "" && inst.ARN == id.ARN {
		return KeyARN, true, false, false
	}
	if inst.Identifier != "" && strings.EqualFold(inst.Identifier, id.Identifier) {
		return KeyIdentifier, true, false, false
	}
	return "", false, false, false
}

// rank orders source statuses from best to worst.
func rank(s SourceStatus) int {
	switch s {
	case SourceOK:
		return 0
	case SourcePartial:
		return 1
	default:
		return 2
	}
}
